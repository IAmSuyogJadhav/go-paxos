package roles

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/go-paxos/domain"
	"github.com/go-paxos/logger"
	"github.com/tryfix/log"
	"io/ioutil"
	"net/http"
	"strconv"
	"sync"
	"time"
)

const (
	typePrepare = `prepare`
	typeAccept  = `accept`

	errInvalidSlotLeader = `leader received a request for an invalid slot`
	errBroadcast         = `sending decision to replicas failed`
	errRequestAcceptor   = `received non-2xx code for acceptor response`
	errInvalidProposal   = `acceptor received an older proposal`
)

type state struct {
	id   int
	slot int
	val  string
}

type Leader struct {
	id       int
	lastSlot int
	promised state
	accepted state
	replicas []string
	leaders  []string // except this one
	client   *http.Client
	lock     *sync.Mutex
	logger   log.Logger
}

func NewLeader() *Leader {
	return &Leader{
		lastSlot: -1,
	}
}

// Propose creates the proposal when a replica has requested this leader and carries out the consensus algorithm
func (l *Leader) Propose(req domain.Request) (ok bool, err error) {
	// return if the requested slot id is not for the next slot
	if l.lastSlot+1 != req.SlotID {
		return false, logger.ErrorWithLine(errors.New(fmt.Sprintf(`%s (slot: %d, requested: %d)`, errInvalidSlotLeader, l.lastSlot+1, req.SlotID)))
	}

	prop, err := l.newProposal(req.SlotID, req.Val)
	if err != nil {
		return false, logger.ErrorWithLine(err)
	}

	resList, err := l.send(typePrepare, prop)
	if err != nil {
		return false, logger.ErrorWithLine(err)
	}

	accepted, rejected, valid := l.validatePromises(resList)
	if valid {
		if accepted > rejected {
			resList, err = l.send(typeAccept, prop)
			if err != nil {
				return false, logger.ErrorWithLine(err)
			}

			accepted, rejected = l.validateAccepts(resList)
			if accepted > rejected {
				var dec domain.Decision
				dec.SlotID = req.SlotID
				dec.Val = req.Val
				l.lastSlot++
				err = l.broadcastDecision(dec, req.Replica)
				if err != nil {
					return false, logger.ErrorWithLine(err)
				}
				return true, nil
			}
		}
	}

	return false, nil
}

// newProposal creates a proposal with an id in the format of `timestamp`+`leader_id`
func (l *Leader) newProposal(slotID int, val string) (domain.Proposal, error) {
	ts := time.Now().Second()
	pId, err := strconv.Atoi(fmt.Sprintf(`%d%d`, ts, l.id))
	if err != nil {
		return domain.Proposal{}, logger.ErrorWithLine(err)
	}

	return domain.Proposal{ID: pId, SlotID: slotID, Val: val}, nil
}

// Broadcasts the decision to all the replicas excluding the requested one
func (l *Leader) broadcastDecision(dec domain.Decision, requester string) error {
	data, err := json.Marshal(dec)
	if err != nil {
		return logger.ErrorWithLine(err)
	}

	for _, replica := range l.replicas {
		if replica == requester {
			continue
		}

		// todo can do in parallel
		req, err := http.NewRequest(http.MethodPost, `http://`+replica+domain.UpdateReplicaEndpoint, bytes.NewBuffer(data))
		if err != nil {
			return logger.ErrorWithLine(err)
		}

		res, err := l.client.Do(req)
		if err != nil {
			return logger.ErrorWithLine(err)
		}

		if res.StatusCode != http.StatusOK {
			res.Body.Close()
			return logger.ErrorWithLine(errors.New(fmt.Sprintf(`%s (status: %d)`, errBroadcast, res.StatusCode)))
		}
		res.Body.Close()
	}

	return nil
}

// Sends out the proposal to all acceptors in both phases prepare and accept, excluding the current leader as it does not exist in leader list
func (l *Leader) send(typ string, prop domain.Proposal) ([]domain.Acceptance, error) {
	data, err := json.Marshal(prop)
	if err != nil {
		return nil, logger.ErrorWithLine(err)
	}

	var endpoint string
	if typ == typePrepare {
		endpoint = domain.PrepareEndpoint
	} else {
		endpoint = domain.AcceptEndpoint
	}

	var resList []domain.Acceptance
	for _, acceptor := range l.leaders {
		// todo do this in parallel
		req, err := http.NewRequest(http.MethodPost, `http://`+acceptor+endpoint, bytes.NewBuffer(data))
		if err != nil {
			return nil, logger.ErrorWithLine(err)
		}

		// todo majority is enough
		res, err := l.client.Do(req)
		if err != nil {
			return nil, logger.ErrorWithLine(err)
		}

		if res.StatusCode != http.StatusOK {
			res.Body.Close()
			return nil, logger.ErrorWithLine(errors.New(fmt.Sprintf(`%s (type: %s, status: %d)`, errRequestAcceptor, typ, res.StatusCode)))
		}

		resData, err := ioutil.ReadAll(res.Body)
		if err != nil {
			res.Body.Close()
			return nil, logger.ErrorWithLine(err)
		}
		res.Body.Close()

		var response domain.Acceptance
		err = json.Unmarshal(resData, &response)
		if err != nil {
			return nil, logger.ErrorWithLine(err)
		}
		resList = append(resList, response)
	}

	return resList, nil
}

// Validates promises upon receiving them from acceptors and returns number of accepted and rejected cases. This function
// returns false for valid if a different proposer has already started a proposal with a higher id.
func (l *Leader) validatePromises(resList []domain.Acceptance) (accepted, rejected int, valid bool) {
	accepted, rejected = 0, 0
	for _, promise := range resList {
		if promise.PrvAccept.Exists {
			if promise.PrvAccept.ID >= promise.PID {
				return accepted, rejected, false
			}
			rejected++
			continue
		}

		if promise.PrvPromise.Exists {
			if promise.PrvPromise.ID >= promise.PID {
				return accepted, rejected, false
			}
			rejected++
			continue
		}
		accepted++
	}

	return accepted, rejected, true
}

// Validates accept responses and returns the accepted and rejected cases
func (l *Leader) validateAccepts(resList []domain.Acceptance) (accepted, rejected int) {
	accepted, rejected = 0, 0
	for _, accept := range resList {
		if accept.Accepted {
			accepted++
			continue
		}
		rejected++
	}

	return accepted, rejected
}

// HandlePrepare handles prepare message requested by a proposer to check if this acceptor has already promised or accepted a proposal
func (l *Leader) HandlePrepare(prop domain.Proposal) (domain.Acceptance, error) {
	var res domain.Acceptance
	res.PID = prop.ID
	l.lock.Lock()
	defer l.lock.Unlock()

	// returns an error if the proposal is for an older slot
	if l.accepted.slot > prop.SlotID {
		return domain.Acceptance{}, logger.ErrorWithLine(errors.New(fmt.Sprintf(`%s (phase: %s, last: %d, requested: %d)`,
			errInvalidProposal, typePrepare, l.accepted.slot, prop.SlotID)))
	}

	if l.promised.slot == prop.SlotID {
		// check if promised id is higher than the requested one since proposer will use this to terminate its proposal
		if l.promised.id >= prop.ID {
			res.PrvPromise.Exists = true
			res.PrvPromise.ID = l.promised.id
			res.PrvPromise.Val = l.promised.val
		} else {
			// as the requested prepare is valid, acceptor updates its state for the same slot
			l.promised.id = prop.ID
			l.promised.val = prop.Val
		}
	} else {
		// if the prepare request is for a new slot
		l.promised.id = prop.ID
		l.promised.slot = prop.SlotID
		l.promised.val = prop.Val
	}

	// if there's an already accepted proposal for the same slot, acceptor just notifies the proposer
	if l.accepted.slot == prop.SlotID && l.accepted.id != 0 {
		res.PrvAccept.Exists = true
		res.PrvAccept.ID = l.accepted.id
		res.PrvAccept.Val = l.accepted.val
	}

	return res, nil
}

// HandleAccept checks if it can accept the confirmation request from a proposer
func (l *Leader) HandleAccept(prop domain.Proposal) (domain.Acceptance, error) {
	// returns an error if the proposal is for an older slot
	if l.accepted.slot > prop.SlotID {
		return domain.Acceptance{}, logger.ErrorWithLine(errors.New(fmt.Sprintf(`%s (phase: %s, last: %d, requested: %d)`,
			errInvalidProposal, typeAccept, l.accepted.slot, prop.SlotID)))
	}

	var res domain.Acceptance
	res.PID = prop.ID
	l.lock.Lock()
	defer l.lock.Unlock()

	// rejects if already promised to a proposal with a higher id for the same slot
	if l.promised.slot == prop.SlotID && l.promised.id >= prop.ID {
		res.Accepted = false
		return res, nil
	}

	// rejects if already accepted for the same slot
	if l.accepted.slot == prop.SlotID && l.accepted.id != 0 {
		res.Accepted = false
		return res, nil
	}

	l.accepted.id = prop.ID
	l.accepted.val = prop.Val
	l.accepted.slot = prop.SlotID
	l.lastSlot++
	res.Accepted = true

	return res, nil
}
