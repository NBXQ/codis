// Copyright 2016 CodisLabs. All Rights Reserved.
// Licensed under the MIT (MIT-LICENSE.txt) license.

package proxy

import (
	"fmt"
	"sync"

	"github.com/CodisLabs/codis/pkg/models"
	"github.com/CodisLabs/codis/pkg/proxy/redis"
	"github.com/CodisLabs/codis/pkg/utils/errors"
	"github.com/CodisLabs/codis/pkg/utils/log"
)

type Slot struct {
	id   int
	lock struct {
		hold bool
		sync.RWMutex
	}
	refs sync.WaitGroup

	backend *SharedBackendConn
	migrate *SharedBackendConn
	replica [][]*SharedBackendConn
}

func (s *Slot) model() *models.Slot {
	var m = &models.Slot{
		Id:          s.id,
		BackendAddr: s.backend.Addr(),
		MigrateFrom: s.migrate.Addr(),
		Locked:      s.lock.hold,
	}
	for i := range s.replica {
		var group []string
		for _, bc := range s.replica[i] {
			group = append(group, bc.Addr())
		}
		m.ReplicaGroup = append(m.ReplicaGroup, group)
	}
	return m
}

func (s *Slot) blockAndWait() {
	if !s.lock.hold {
		s.lock.hold = true
		s.lock.Lock()
	}
	s.refs.Wait()
}

func (s *Slot) unblock() {
	if !s.lock.hold {
		return
	}
	s.lock.hold = false
	s.lock.Unlock()
}

func (s *Slot) forward(fn dispFunc, r *Request, key []byte) error {
	s.lock.RLock()
	bc, err := s.prepare(fn, r, key)
	s.lock.RUnlock()
	if err != nil {
		return err
	} else {
		bc.PushBack(r)
		return nil
	}
}

var (
	ErrSlotIsNotReady = errors.New("slot is not ready, may be offline")
	ErrRespIsRequired = errors.New("resp is required")
)

func (s *Slot) prepare(fn dispFunc, r *Request, key []byte) (*SharedBackendConn, error) {
	if s.backend == nil {
		log.Warnf("slot-%04d is not ready: key = %s", s.id, key)
		return nil, ErrSlotIsNotReady
	}
	if err := s.slotsmgrt(r, key); err != nil {
		log.Warnf("slot-%04d migrate from = %s to %s failed: key = %s, error = %s",
			s.id, s.migrate.Addr(), s.backend.Addr(), key, err)
		return nil, err
	} else {
		r.Group = &s.refs
		r.Group.Add(1)
		if fn != nil {
			return fn(s, r), nil
		}
		return s.backend, nil
	}
}

func (s *Slot) slotsmgrt(r *Request, key []byte) error {
	if s.migrate == nil {
		return nil
	}
	if len(key) == 0 {
		return nil
	}

	m := &Request{}
	m.Multi = []*redis.Resp{
		redis.NewBulkBytes([]byte("SLOTSMGRTTAGONE")),
		redis.NewBulkBytes(s.backend.host),
		redis.NewBulkBytes(s.backend.port),
		redis.NewBulkBytes([]byte("3000")),
		redis.NewBulkBytes(key),
	}
	m.Batch = &sync.WaitGroup{}

	s.migrate.PushBack(m)

	m.Batch.Wait()

	if err := m.Err; err != nil {
		return err
	}
	switch resp := m.Resp; {
	case resp == nil:
		return ErrRespIsRequired
	case resp.IsError():
		return fmt.Errorf("error resp: %s", resp.Value)
	case resp.IsInt():
		log.Debugf("slot-%04d migrate from %s to %s: key = %s, resp = %s",
			s.id, s.migrate.Addr(), s.backend.Addr(), key, resp.Value)
		return nil
	default:
		return fmt.Errorf("error resp: should be integer, but got %s", resp.Type)
	}
}
