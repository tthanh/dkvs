package raft

import "fmt"

type InmemLogStore struct {
	entries []*Log
}

func NewInmemLogStore() *InmemLogStore {
	return &InmemLogStore{
		entries: []*Log{},
	}
}

func (i *InmemLogStore) FirstIndex() (uint64, error) {
	return i.entries[0].Index, nil
}

func (i *InmemLogStore) LastIndex() (uint64, error) {
	l := len(i.entries)
	if l > 0 {
		return i.entries[l-1].Index, nil
	}

	return 0, nil
}

func (i *InmemLogStore) GetLog(idx uint64) (*Log, error) {
	for _, entry := range i.entries {
		if entry.Index == idx {
			return entry, nil
		}
	}
	return nil, fmt.Errorf("Can't get log witn index %d", idx)
}

func (i *InmemLogStore) SetLog(entry *Log) error {
	i.entries = append(i.entries, entry)
	return nil
}

func (i *InmemLogStore) SetLogs(entries []*Log) error {
	for _, entry := range entries {
		i.entries = append(i.entries, entry)
	}
	return nil
}

func (i *InmemLogStore) DeleteRange(min, max uint64) error {
	for j := min; j < max; j++ {
		for _, entry := range i.entries {
			if entry.Index == j {
				i.entries = append(i.entries[:j], i.entries[j+1:]...)
			}
		}
	}
	return nil
}