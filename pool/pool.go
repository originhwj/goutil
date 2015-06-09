// Copyright 2012 Gary Burd
// Modified by Wesdu (2015)
//
// Licensed under the Apache License, Version 2.0 (the "License"): you may
// not use this file except in compliance with the License. You may obtain
// a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS, WITHOUT
// WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the
// License for the specific language governing permissions and limitations
// under the License.

package pool

import (
	"bufio"
	"container/list"
	"errors"
	"net"
	"sync"
	"time"
)

const (
	CRLF = "\r\n"
)

type Conn interface {
	Close() error
	Err() error
	ReadBytesLine () ([]byte, error)
	WriteStringLine(string) error
}

type conn struct {
	raw_c net.Conn
	mu    sync.Mutex
	err   error
	br    *bufio.Reader
	bw    *bufio.Writer
}

func Dial(network, address string) (Conn, error) {
	netConn, err := net.Dial(network, address)
	if err != nil {
		return nil, err
	}
	return &conn{
		raw_c: netConn,
		bw:    bufio.NewWriter(netConn),
		br:    bufio.NewReader(netConn),
	}, nil
}

func (c *conn) ReadBytesLine() ([]byte, error) {
	p, err := c.br.ReadSlice('\n')
	if err == bufio.ErrBufferFull {
		return nil, errors.New("pool: long response line")
	}
	if err != nil {
		return nil, err
	}
	i := len(p) - 2
	if i < 0 || p[i] != '\r' {
		return nil, errors.New("pool: bad response line terminator")
	}
	return p[:i], nil
}

func (c *conn) WriteStringLine(s string) error{
	c.bw.WriteString(s)
	_, err := c.bw.WriteString(CRLF)
	if err != nil {
		return err
	}
	err = c.bw.Flush()
	return err
}

func (c *conn) fatal(err error) error {
	c.mu.Lock()
	if c.err == nil {
		c.err = err
		c.raw_c.Close()
	}
	c.mu.Unlock()
	return err
}

func (c *conn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	err := c.err
	if c.err == nil {
		c.err = errors.New("pool: closed")
		err = c.raw_c.Close()
	}
	return err
}
func (c *conn) Err() error {
	c.mu.Lock()
	err := c.err
	c.mu.Unlock()
	return err
}

type idleConn struct {
	c Conn
	t time.Time
}

type pooledConnection struct {
	p *Pool
	Conn
}

func (pc *pooledConnection) Close() error {
	return pc.p.put(pc.Conn, false)
}

func (pc *pooledConnection) ForceClose() error {
	return pc.p.put(pc.Conn, true)
}

type Pool struct {
	Dial         func() (Conn, error)
	TestOnBorrow func(c Conn, t time.Time) error
	IdleTimeout  time.Duration
	MaxIdle      int
	MaxActive    int
	mu           sync.Mutex
	closed       bool
	idle         list.List
	active       int
	cond         *sync.Cond
	Wait         bool
}

func (p *Pool) release() {
	p.active -= 1
	if p.cond != nil {
		p.cond.Signal()
	}
}

func (p *Pool) Get() (Conn, error) {
	c, err := p.get()
	return &pooledConnection{p: p, Conn: c}, err
}

func (p *Pool) get() (Conn, error) {
	p.mu.Lock()

	if timeout := p.IdleTimeout; timeout > 0 {
		for e := p.idle.Back(); e != nil; e = e.Prev() {
			ic := e.Value.(idleConn)
			if ic.t.Add(timeout).After(time.Now()) {
				break
			}
			p.idle.Remove(e)
			p.release()
			p.mu.Unlock()
			ic.c.Close()
			p.mu.Lock()
		}
	}

	for {

		for e := p.idle.Front(); e != nil; e = e.Next() {
			ic := e.Value.(idleConn)
			p.idle.Remove(e)
			test := p.TestOnBorrow
			p.mu.Unlock()
			if test == nil || test(&pooledConnection{p: p, Conn: ic.c}, ic.t) == nil {
				return ic.c, nil
			}
			ic.c.Close()
			p.mu.Lock()
			p.release()
		}

		if p.closed {
			p.mu.Unlock()
			return nil, errors.New("pool: get on closed pool")
		}

		if p.MaxActive == 0 || p.active < p.MaxActive {
			dial := p.Dial
			p.active += 1
			p.mu.Unlock()
			c, err := dial()
			if err != nil {
				p.mu.Lock()
				p.release()
				p.mu.Unlock()
				c = nil
			}
			return c, err
		}

		if !p.Wait {
			p.mu.Unlock()
			return nil, errors.New("pool: connection pool exhausted")
		}

		if p.cond == nil {
			p.cond = sync.NewCond(&p.mu)
		}
		p.cond.Wait()
	}
}

func (p *Pool) put(c Conn, forceClose bool) error {
	err := c.Err()
	p.mu.Lock()
	if !p.closed && err == nil && !forceClose {
		p.idle.PushFront(idleConn{t: time.Now(), c: c})
		if p.idle.Len() > p.MaxIdle {
			c = p.idle.Remove(p.idle.Back()).(idleConn).c
		} else {
			c = nil
		}
	}
	p.release()
	p.mu.Unlock()
	if c == nil {
		return nil
	} else {
		return c.Close()
	}
}

func (p *Pool) Close() error {
	p.mu.Lock()
	idle := p.idle
	idle.Init()
	p.closed = true
	p.active -= idle.Len()
	if p.cond != nil {
		p.cond.Broadcast()
	}
	p.mu.Unlock()
	for e := idle.Front(); e != nil; e = e.Next() {
		e.Value.(idleConn).c.Close()
	}
	return nil
}

