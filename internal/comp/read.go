package comp

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/Raider-Mate/raider-mate-service/internal/db"
)

// CompInfo is one named comp's mode, without its slots.
type CompInfo struct {
	Name string
	Mode db.CompMode
}

// Unseated is one raider in the assignment pool who holds no slot on a board.
//
// Reason is the sentence to show, the same contract Assignment.Reason has: the reader
// works out why somebody is not playing so no client has to.
type Unseated struct {
	CharacterID uuid.UUID
	Status      db.SignupStatus
	SignedUpAt  time.Time
	Reason      string
}

// Board is one named comp's mode and full slot list.
//
// Unseated is the other half of the answer. A board is the snapshot the last lock took,
// signups carry on afterwards, and a raider who arrived since then holds no slot: without
// this they are on the event and on no board, with nothing anywhere saying so.
type Board struct {
	Name  string
	Mode  db.CompMode
	Slots []Assignment
	// Advisories are what the board says about itself, worked out from the slots on
	// every read. See Advise for why they are not the ones the lock reported.
	Advisories []Advisory
	Unseated   []Unseated
}

// readerStore is the persistence Reader needs. Declared here, by the consumer.
type readerStore interface {
	ListComps(ctx context.Context, eventID uuid.UUID) ([]CompInfo, error)
	CompMode(ctx context.Context, eventID uuid.UUID, compName string) (db.CompMode, bool, error)
	ListCompSlots(ctx context.Context, eventID uuid.UUID, compName string) ([]Assignment, error)
	ListUnseated(ctx context.Context, eventID uuid.UUID, compName string) ([]Unseated, error)
	CompShape(ctx context.Context, eventID uuid.UUID) (Template, Mode, error)
}

// Reader is read-only access to comps and their slots.
type Reader struct {
	store readerStore
}

// NewReader builds a Reader.
func NewReader(store readerStore) *Reader {
	return &Reader{store: store}
}

// List returns every named comp for an event.
func (r *Reader) List(ctx context.Context, eventID uuid.UUID) ([]CompInfo, error) {
	infos, err := r.store.ListComps(ctx, eventID)
	if err != nil {
		return nil, fmt.Errorf("listing comps: %w", err)
	}
	return infos, nil
}

// Get returns one named comp's mode and slots. found is false when no comp by that
// name has ever been created for the event.
func (r *Reader) Get(ctx context.Context, eventID uuid.UUID, name string) (board Board, found bool, err error) {
	mode, found, err := r.store.CompMode(ctx, eventID, name)
	if err != nil {
		return Board{}, false, fmt.Errorf("reading comp mode: %w", err)
	}
	if !found {
		return Board{}, false, nil
	}

	slots, err := r.store.ListCompSlots(ctx, eventID, name)
	if err != nil {
		return Board{}, false, fmt.Errorf("listing comp slots: %w", err)
	}

	unseated, err := r.store.ListUnseated(ctx, eventID, name)
	if err != nil {
		return Board{}, false, fmt.Errorf("listing unseated raiders: %w", err)
	}

	tmpl, sizing, err := r.store.CompShape(ctx, eventID)
	if err != nil {
		return Board{}, false, fmt.Errorf("reading comp shape: %w", err)
	}

	return Board{
		Name: name, Mode: mode, Slots: slots,
		Advisories: Advise(slots, tmpl, sizing),
		Unseated:   unseated,
	}, true, nil
}
