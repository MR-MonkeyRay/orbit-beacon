package identity

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"

	"orbitbeacon/internal/agent/filelock"
)

const maxTicketAliases = 64

// Enrollment retains the exact candidate and CAS parent of a ticket operation.
// It is separate from Identity so snapshots do not expose or copy the journal.
type Enrollment struct {
	NodeToken         string `json:"node_token"`
	PreviousNodeToken string `json:"previous_node_token,omitempty"`
}

type diskIdentity struct {
	Identity
	EnrollmentAttempts map[string]Enrollment `json:"enrollment_attempts,omitempty"`
}

// LookupEnrollment reads an existing operation without allocating a candidate.
func (s *Store) LookupEnrollment(ctx context.Context, ticket [32]byte) (Identity, Enrollment, bool, error) {
	unlock, err := filelock.Acquire(ctx, s.path+".lock")
	if err != nil {
		return Identity{}, Enrollment{}, false, err
	}
	defer unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Identity{}, Enrollment{}, false, err
	}
	current, err := load(s.path)
	if err != nil {
		return Identity{}, Enrollment{}, false, err
	}
	operation, exists := current.EnrollmentAttempts[hex.EncodeToString(ticket[:])]
	s.current = current.Identity
	return current.Identity, operation, exists, nil
}

// StageEnrollment durably binds a ticket to its candidate before any claim.
// Rotation may be requested before a response has supplied the node binding.
func (s *Store) StageEnrollment(ctx context.Context, ticket [32]byte, rotate bool) (Identity, Enrollment, error) {
	return s.prepareEnrollment(ctx, ticket, rotate, "")
}

// RememberEnrollment repairs a missing association only for a credential still
// present in durable identity, after a read-only server receipt confirmation.
func (s *Store) RememberEnrollment(ctx context.Context, ticket [32]byte, accepted string) (Identity, Enrollment, error) {
	if accepted == "" {
		return Identity{}, Enrollment{}, errors.New("confirmed enrollment credential is missing")
	}
	return s.prepareEnrollment(ctx, ticket, false, accepted)
}

func (s *Store) prepareEnrollment(ctx context.Context, ticket [32]byte, rotate bool, accepted string) (Identity, Enrollment, error) {
	unlock, err := filelock.Acquire(ctx, s.path+".lock")
	if err != nil {
		return Identity{}, Enrollment{}, err
	}
	defer unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Identity{}, Enrollment{}, err
	}
	current, err := load(s.path)
	if err != nil {
		return Identity{}, Enrollment{}, err
	}
	hash := hex.EncodeToString(ticket[:])
	if operation, exists := current.EnrollmentAttempts[hash]; exists {
		if accepted != "" && operation.NodeToken != accepted {
			return Identity{}, Enrollment{}, errors.New("confirmed enrollment differs from its durable operation")
		}
		s.current = current.Identity
		return current.Identity, operation, nil
	}
	operation := Enrollment{}
	if accepted != "" {
		if accepted != current.NodeToken && accepted != current.PendingNodeToken {
			return Identity{}, Enrollment{}, errors.New("confirmed enrollment was superseded locally")
		}
		operation.NodeToken = accepted
		if accepted == current.PendingNodeToken {
			operation.PreviousNodeToken = current.NodeToken
		}
	} else if current.PendingNodeToken != "" {
		operation = Enrollment{NodeToken: current.PendingNodeToken, PreviousNodeToken: current.NodeToken}
	} else if rotate || current.NodeID != "" {
		current.PendingNodeToken, err = randomID(s.random, 32)
		if err != nil {
			return Identity{}, Enrollment{}, err
		}
		if current.PendingNodeToken == current.NodeToken {
			return Identity{}, Enrollment{}, errors.New("identity entropy repeated active credential")
		}
		operation = Enrollment{NodeToken: current.PendingNodeToken, PreviousNodeToken: current.NodeToken}
	} else {
		operation.NodeToken = current.NodeToken
	}
	aliases := 0
	for _, existing := range current.EnrollmentAttempts {
		if existing.NodeToken == operation.NodeToken {
			aliases++
		}
	}
	if aliases >= maxTicketAliases || len(current.EnrollmentAttempts) >= 2*maxTicketAliases {
		return Identity{}, Enrollment{}, errors.New("too many unresolved enrollment ticket operations")
	}
	if current.EnrollmentAttempts == nil {
		current.EnrollmentAttempts = make(map[string]Enrollment)
	}
	current.EnrollmentAttempts[hash] = operation
	if err := persist(s.path, current, false); err != nil {
		return Identity{}, Enrollment{}, err
	}
	s.current = current.Identity
	return current.Identity, operation, nil
}

func pruneEnrollmentAttempts(current *diskIdentity) {
	for hash, operation := range current.EnrollmentAttempts {
		if operation.NodeToken != current.NodeToken && operation.NodeToken != current.PendingNodeToken {
			delete(current.EnrollmentAttempts, hash)
		}
	}
}

func validateEnrollmentAttempts(current diskIdentity) error {
	if len(current.EnrollmentAttempts) > 2*maxTicketAliases {
		return errors.New("too many enrollment ticket operations")
	}
	active, pending := 0, 0
	for hash, operation := range current.EnrollmentAttempts {
		if len(hash) != 64 {
			return errors.New("invalid enrollment ticket digest")
		}
		for _, b := range []byte(hash) {
			if (b < '0' || b > '9') && (b < 'a' || b > 'f') {
				return errors.New("invalid enrollment ticket digest")
			}
		}
		if operation.NodeToken == current.NodeToken {
			active++
		} else if current.PendingNodeToken != "" && operation.NodeToken == current.PendingNodeToken && operation.PreviousNodeToken == current.NodeToken {
			pending++
		} else {
			return errors.New("enrollment operation no longer belongs to durable credentials")
		}
		if operation.PreviousNodeToken != "" {
			var decoded [32]byte
			if len(operation.PreviousNodeToken) != 43 || operation.PreviousNodeToken == operation.NodeToken {
				return errors.New("invalid enrollment operation parent")
			}
			if n, err := base64.RawURLEncoding.Decode(decoded[:], []byte(operation.PreviousNodeToken)); err != nil || n != len(decoded) {
				return errors.New("invalid enrollment operation parent")
			}
		}
	}
	if active > maxTicketAliases || pending > maxTicketAliases {
		return errors.New("too many aliases for an enrollment credential")
	}
	return nil
}
