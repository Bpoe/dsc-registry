package database

import "github.com/Bpoe/dsc-registry/internal/eventing"

func (s *Store) notify(kind string, data eventing.Identifiers) {
	if s.Events != nil {
		s.Events.Notify(kind, data)
	}
}
