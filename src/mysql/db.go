package mysql

import (
	"container/list"
	"lib/log"
	"sync"
)

type DB struct {
	addr     string
	user     string
	password string
	db       string

	maxIdleConns int

	sync.Mutex

	conns *list.List
}

type dbConn struct {
	sync.Mutex
	*conn

	stmts map[*stmt]bool

	closed bool
}

func (c *dbConn) Close() {
	if c.closed {
		return
	}

	c.closed = true
	c.conn.Close()
}

func NewDB(addr string, user string, password string, db string, maxIdleConns int) *DB {
	d := new(DB)

	d.addr = addr
	d.user = user
	d.password = password
	d.db = db
	d.maxIdleConns = maxIdleConns

	d.conns = list.New()

	return d
}

func (db *DB) newConn() (*dbConn, error) {
	co := new(conn)

	if err := co.Connect(db.addr, db.user, db.password, db.db); err != nil {
		log.Error("connect %s error %s", db.addr, err.Error())
		return nil, err
	}

	dc := new(dbConn)
	dc.conn = co
	dc.closed = false

	dc.stmts = make(map[*stmt]bool)

	return dc, nil
}

func (db *DB) tryReuse(co *dbConn) error {
	if co.isInTransaction() {
		//we can not reuse a connection in transaction status
		log.Warn("reuse connection can not in transaction status, rollback")
		if err := co.Rollback(); err != nil {
			return err
		}
	} else if !co.isAutoCommit() {
		//we can not  reuse a connection not in autocomit
		log.Warn("reuse connection must have autocommit status, enable autocommit")
		if _, err := co.Exec("set autocommit = 1"); err != nil {
			return err
		}
	}
	return nil
}

func (db *DB) popConn() (co *dbConn, err error) {
	db.Lock()
	if db.conns.Len() > 0 {
		v := db.conns.Back()
		co = v.Value.(*dbConn)
		db.conns.Remove(v)
	}
	db.Unlock()

	if co != nil {
		co.Lock()
		if err := co.Ping(); err == nil {
			if err := db.tryReuse(co); err == nil {
				co.Unlock()
				//connection may alive
				return co, nil
			}
		}

		co.Close()
		co.Unlock()
	}

	return db.newConn()
}

func (db *DB) pushConn(co *dbConn, err error) {
	var closeConn *dbConn = nil

	if err == ErrBadConn {
		closeConn = co
	} else {
		db.Lock()

		if db.conns.Len() >= db.maxIdleConns {
			closeConn = co
		} else {
			db.conns.PushBack(co)
		}

		db.Unlock()

	}

	if closeConn != nil {
		closeConn.Lock()
		closeConn.Close()
		closeConn.Unlock()
	}
}

func (db *DB) Ping() (err error) {
	var c *dbConn
	for i := 0; i < 3; i++ {
		c, err = db.popConn()
		if err != nil {
			return
		}

		c.Lock()
		err = c.Ping()
		c.Unlock()

		db.pushConn(c, err)

		if err != ErrBadConn {
			break
		}
	}
	return
}

func (db *DB) Exec(query string, args ...interface{}) (r *Result, err error) {
	for i := 0; i < 10; i++ {
		if r, err = db.exec(query, args...); err != ErrBadConn {
			break
		}
	}
	return
}

func (db *DB) exec(query string, args ...interface{}) (r *Result, err error) {
	var c *dbConn
	c, err = db.popConn()
	if err != nil {
		return
	}

	c.Lock()
	r, err = c.Exec(query, args...)
	c.Unlock()

	db.pushConn(c, err)
	return
}

func (db *DB) Query(query string, args ...interface{}) (r *Resultset, err error) {
	for i := 0; i < 10; i++ {
		if r, err = db.query(query, args...); err != ErrBadConn {
			break
		}
	}
	return
}

func (db *DB) query(query string, args ...interface{}) (r *Resultset, err error) {
	var c *dbConn
	c, err = db.popConn()
	if err != nil {
		return
	}

	c.Lock()
	r, err = c.Query(query, args...)
	c.Unlock()

	db.pushConn(c, err)
	return
}

func (db *DB) Prepare(query string) (s *Stmt, err error) {
	s = newStmt(db, query)

	var c *dbConn
	for i := 0; i < 10; i++ {
		c, _, err = s.prepare(query)
		db.pushConn(c, err)
		if err != ErrBadConn {
			break
		}
	}
	return
}

func (db *DB) Begin() (t *Tx, err error) {
	t = new(Tx)

	t.db = db
	t.done = false

	var conn *dbConn

	for i := 0; i < 10; i++ {
		if conn, err = db.begin(); err == nil {
			t.conn = conn
			return
		} else {
			db.pushConn(conn, err)
		}

		if err != ErrBadConn {
			break
		}
	}

	return
}

func (db *DB) begin() (conn *dbConn, err error) {
	if conn, err = db.popConn(); err != nil {
		return
	}

	conn.Lock()
	err = conn.Begin()
	conn.Unlock()
	return
}

//for mysql stmt test, stmt is global to session
//so when a transaction prepare a stmt, it's exists after transaction over.

type Stmt struct {
	db  *DB
	str string

	stmts map[*dbConn]*stmt

	//in transaction
	txStmt *stmt
	tx     *Tx
}

func newStmt(db *DB, query string) *Stmt {
	s := new(Stmt)

	s.db = db
	s.str = query
	s.stmts = make(map[*dbConn]*stmt)

	s.txStmt = nil
	s.tx = nil

	return s
}

func (s *Stmt) txQuery(args ...interface{}) (*Resultset, error) {
	if s.tx.done {
		s.txClose()
		return nil, ErrTxDone
	}

	c := s.tx.conn

	c.Lock()
	r, err := s.txStmt.Query(args...)
	c.Unlock()

	return r, err
}

func (s *Stmt) txExec(args ...interface{}) (*Result, error) {
	if s.tx.done {
		s.txClose()

		return nil, ErrTxDone
	}

	c := s.tx.conn

	c.Lock()
	r, err := s.txStmt.Exec(args...)
	c.Unlock()

	return r, err
}

func (s *Stmt) prepare(query string) (conn *dbConn, st *stmt, err error) {
	conn, err = s.db.popConn()
	if err != nil {
		return
	}

	var ok bool = false
	if st, ok = s.stmts[conn]; ok {
		return
	}

	conn.Lock()
	st, err = conn.Prepare(query)
	conn.Unlock()

	if err == nil {
		s.stmts[conn] = st
	}
	return

}

func (s *Stmt) Exec(args ...interface{}) (r *Result, err error) {
	if s.tx != nil {
		if r, err = s.txExec(args...); err == nil {
			return
		} else if err != ErrTxDone {
			return
		}

		//if err is ErrTxDone, we will use other conn
	}

	for i := 0; i < 10; i++ {
		if r, err = s.exec(args...); err != ErrBadConn {
			break
		}
	}
	return

}

func (s *Stmt) exec(args ...interface{}) (*Result, error) {
	if c, st, err := s.prepare(s.str); err != nil {
		s.db.pushConn(c, err)
		return nil, err
	} else {
		var r *Result
		c.Lock()
		r, err = st.Exec(args...)
		c.Unlock()
		s.db.pushConn(c, err)
		return r, err
	}
}

func (s *Stmt) Query(args ...interface{}) (r *Resultset, err error) {
	if s.tx != nil {
		if r, err = s.txQuery(args...); err == nil {
			return
		} else if err != ErrTxDone {
			return
		}

		//if err is ErrTxDone, we will use other conn
	}

	for i := 0; i < 10; i++ {
		if r, err = s.query(args...); err != ErrBadConn {
			break
		}
	}
	return

}

func (s *Stmt) query(args ...interface{}) (*Resultset, error) {
	if c, st, err := s.prepare(s.str); err != nil {
		s.db.pushConn(c, err)
		return nil, err
	} else {
		var r *Resultset
		c.Lock()
		r, err = st.Query(args...)
		c.Unlock()
		s.db.pushConn(c, err)
		return r, err
	}
}

func (s *Stmt) txClose() (err error) {
	c := s.tx.conn
	c.Lock()
	if !c.closed {
		err = s.txStmt.Close()
	}
	c.Unlock()
	s.tx = nil
	return

}

func (s *Stmt) Close() (err error) {
	if s.tx != nil {
		return s.txClose()
	}

	for c, st := range s.stmts {
		c.Lock()
		if !c.closed {
			err = st.Close()
		}
		c.Unlock()
	}

	s.stmts = map[*dbConn]*stmt{}
	return
}

type Tx struct {
	sync.Mutex
	db   *DB
	done bool
	conn *dbConn
}

func (t *Tx) Exec(query string, args ...interface{}) (*Result, error) {
	if t.done {
		return nil, ErrTxDone
	}

	t.conn.Lock()
	r, err := t.conn.Exec(query, args...)
	t.conn.Unlock()
	return r, err
}

func (t *Tx) Query(query string, args ...interface{}) (*Resultset, error) {
	if t.done {
		return nil, ErrTxDone
	}

	t.conn.Lock()
	r, err := t.conn.Query(query, args...)
	t.conn.Unlock()
	return r, err
}

func (t *Tx) Prepare(query string) (*Stmt, error) {
	if t.done {
		return nil, ErrTxDone
	}

	s := newStmt(t.db, query)

	t.conn.Lock()
	st, err := t.conn.Prepare(query)
	t.conn.Unlock()

	if err != nil {
		return nil, err
	}

	s.tx = t
	s.txStmt = st

	return s, nil
}

func (t *Tx) Commit() error {
	if t.done {
		return ErrTxDone
	}

	t.conn.Lock()
	err := t.conn.Commit()
	t.conn.Unlock()

	t.db.pushConn(t.conn, err)

	t.done = true

	return err
}

func (t *Tx) Rollback() error {
	if t.done {
		return ErrTxDone
	}

	t.conn.Lock()
	err := t.conn.Commit()
	t.conn.Unlock()

	t.db.pushConn(t.conn, err)

	t.done = true

	return err
}
