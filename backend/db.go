// Copyright 2016 The kingshard Authors. All rights reserved.
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

package backend

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/flike/kingshard/core/errors"
	"github.com/flike/kingshard/mysql"
)

const (
	Up = iota
	Down
	ManualDown
	Unknown

	InitConnCount           = 16
	DefaultMaxConnNum       = 1024
	PingPeroid        int64 = 4
)

type DB struct {
	sync.RWMutex

	addr     string
	user     string
	password string
	db       string
	state    int32

	maxConnNum  int
	InitConnNum int
	idleConns   chan *Conn
	cacheConns  chan *Conn
	checkConn   *Conn
}

func Open(addr string, user string, password string, dbName string, maxConnNum int) (*DB, error) {
	var err error
	db := new(DB)
	db.addr = addr
	db.user = user
	db.password = password
	db.db = dbName

	//如果最大连接数大于零
	if 0 < maxConnNum {
		db.maxConnNum = maxConnNum
		/* 最大连接数如果小于16则无效 */
		if db.maxConnNum < 16 {
			db.InitConnNum = db.maxConnNum
		} else {
			/* 初始连接数为最大连接数的除以4取商 */
			db.InitConnNum = db.maxConnNum / 4
		}
	} else {
		db.maxConnNum = DefaultMaxConnNum
		db.InitConnNum = InitConnCount
	}
	//check connection
	/* 在建立DB连接池之前首先检查是否可以正确建立连接,同时也是后端保活goroutine的检测连接 */
	db.checkConn, err = db.newConn()
	if err != nil {
		db.Close()
		return nil, err
	}

	/* 缓存连接channel,优先使用cache channel中的连接 */
	db.cacheConns = make(chan *Conn, db.maxConnNum)
	/* 空闲连接channel,当cacheConns耗尽时使用idleConns中的连接 */
	db.idleConns = make(chan *Conn, db.maxConnNum)
	atomic.StoreInt32(&(db.state), Unknown)

	for i := 0; i < db.maxConnNum; i++ {
		/* 1/4的已经建立的连接保存在cacheConns */
		if i < db.InitConnNum {
			conn, err := db.newConn()
			if err != nil {
				db.Close()
				return nil, err
			}
			conn.pushTimestamp = time.Now().Unix()
			db.cacheConns <- conn
		} else {
			/* 3/4的初始化连接保存在idleConns,但是未建立连接 */
			conn := new(Conn)
			db.idleConns <- conn
		}
	}

	return db, nil
}

func (db *DB) Addr() string {
	return db.addr
}

func (db *DB) State() string {
	var state string
	switch db.state {
	case Up:
		state = "up"
	case Down, ManualDown:
		state = "down"
	case Unknown:
		state = "unknow"
	}
	return state
}

func (db *DB) IdleConnCount() int {
	db.RLock()
	defer db.RUnlock()
	return len(db.cacheConns)
}

func (db *DB) Close() error {
	db.Lock()
	idleChannel := db.idleConns
	cacheChannel := db.cacheConns
	db.cacheConns = nil
	db.idleConns = nil
	db.Unlock()
	if cacheChannel == nil || idleChannel == nil {
		return nil
	}

	close(cacheChannel)
	for conn := range cacheChannel {
		db.closeConn(conn)
	}
	close(idleChannel)

	return nil
}

func (db *DB) getConns() (chan *Conn, chan *Conn) {
	db.RLock()
	cacheConns := db.cacheConns
	idleConns := db.idleConns
	db.RUnlock()
	return cacheConns, idleConns
}

func (db *DB) getCacheConns() chan *Conn {
	db.RLock()
	conns := db.cacheConns
	db.RUnlock()
	return conns
}

func (db *DB) getIdleConns() chan *Conn {
	db.RLock()
	conns := db.idleConns
	db.RUnlock()
	return conns
}

//通过check connection进行ping，检查后端db是否存活
func (db *DB) Ping() error {
	var err error
	if db.checkConn == nil {
		db.checkConn, err = db.newConn()
		if err != nil {
			db.closeConn(db.checkConn)
			db.checkConn = nil
			return err
		}
	}
	err = db.checkConn.Ping()
	if err != nil {
		db.closeConn(db.checkConn)
		db.checkConn = nil
		return err
	}
	return nil
}

//新建一条连接
func (db *DB) newConn() (*Conn, error) {
	co := new(Conn)

	if err := co.Connect(db.addr, db.user, db.password, db.db); err != nil {
		return nil, err
	}

	return co, nil
}

//关闭连接，连接入备用连接池
func (db *DB) closeConn(co *Conn) error {
	if co != nil {
		co.Close()
		conns := db.getIdleConns()
		if conns != nil {
			select {
			case conns <- co:
				return nil
			default:
				return nil
			}
		}
	}
	return nil
}

func (db *DB) tryReuse(co *Conn) error {
	var err error
	//reuse Connection
	if co.IsInTransaction() {
		//we can not reuse a connection in transaction status
		err = co.Rollback()
		if err != nil {
			return err
		}
	}

	if !co.IsAutoCommit() {
		//we can not  reuse a connection not in autocomit
		_, err = co.exec("set autocommit = 1")
		if err != nil {
			return err
		}
	}

	//connection may be set names early
	//we must use default utf8
	if co.GetCharset() != mysql.DEFAULT_CHARSET {
		err = co.SetCharset(mysql.DEFAULT_CHARSET)
		if err != nil {
			return err
		}
	}

	return nil
}

func (db *DB) PopConn() (*Conn, error) {
	var co *Conn
	var err error

	cacheConns, idleConns := db.getConns()
	if cacheConns == nil || idleConns == nil {
		return nil, errors.ErrDatabaseClose
	}
	co = db.GetConnFromCache(cacheConns)
	if co == nil {
		co, err = db.GetConnFromIdle(cacheConns, idleConns)
		if err != nil {
			return nil, err
		}
	}

	err = db.tryReuse(co)
	if err != nil {
		db.closeConn(co)
		return nil, err
	}

	return co, nil
}

func (db *DB) GetConnFromCache(cacheConns chan *Conn) *Conn {
	var co *Conn
	var err error
	for 0 < len(cacheConns) {
		//从channel中获取连接
		co = <-cacheConns
		//如果保活检测时间小于当前时间间隔，则强制进行存活检测
		if co != nil && PingPeroid < time.Now().Unix()-co.pushTimestamp {
			err = co.Ping()
			if err != nil {
				db.closeConn(co)
				co = nil
			}
		}
		if co != nil {
			break
		}
	}
	return co
}

//从备用idle conns中取出一个初始连接，并建立与后端连接
func (db *DB) GetConnFromIdle(cacheConns, idleConns chan *Conn) (*Conn, error) {
	var co *Conn
	var err error
	select {
	case co = <-idleConns:
		err = co.Connect(db.addr, db.user, db.password, db.db)
		if err != nil {
			db.closeConn(co)
			return nil, err
		}
		return co, nil
	case co = <-cacheConns:
		if co == nil {
			return nil, errors.ErrConnIsNil
		}
		if co != nil && PingPeroid < time.Now().Unix()-co.pushTimestamp {
			err = co.Ping()
			if err != nil {
				db.closeConn(co)
				return nil, errors.ErrBadConn
			}
		}
	}
	return co, nil
}

//回收连接
func (db *DB) PushConn(co *Conn, err error) {
	if co == nil {
		return
	}
	conns := db.getCacheConns()
	if conns == nil {
		co.Close()
		return
	}
	if err != nil {
		db.closeConn(co)
		return
	}
	co.pushTimestamp = time.Now().Unix()
	select {
	case conns <- co:
		return
	default:
		db.closeConn(co)
		return
	}
}

type BackendConn struct {
	*Conn
	db *DB
}

//如果有错误，关闭连接;否则回收连接
func (p *BackendConn) Close() {
	if p != nil && p.Conn != nil {
		if p.Conn.pkgErr != nil {
			p.db.closeConn(p.Conn)
		} else {
			p.db.PushConn(p.Conn, nil)
		}
		p.Conn = nil
	}
}

//从连接池中获取连接
func (db *DB) GetConn() (*BackendConn, error) {
	c, err := db.PopConn()
	if err != nil {
		return nil, err
	}
	return &BackendConn{c, db}, nil
}
