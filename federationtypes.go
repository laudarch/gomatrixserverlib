package gomatrixserverlib

import (
	"encoding/json"
	"fmt"
)

// A RespSend is the content of a response to PUT /_matrix/federation/v1/send/{txnID}/
type RespSend struct {
	// Map of event ID to the result of processing that event.
	PDUs map[string]PDUResult `json:"pdus"`
}

// A PDUResult is the result of processing a matrix room event.
type PDUResult struct {
	// If not empty then this is a human readable description of a problem
	// encountered processing an event.
	Error string `json:"error,omitempty"`
}

// A RespStateIDs is the content of a response to GET /_matrix/federation/v1/state_ids/{roomID}/{eventID}
type RespStateIDs struct {
	// A list of state event IDs for the state of the room before the requested event.
	StateEventIDs []string `json:"pdu_ids"`
	// A list of event IDs needed to authenticate the state events.
	AuthEventIDs []string `json:"auth_chain_ids"`
}

// A RespState is the content of a response to GET /_matrix/federation/v1/state/{roomID}/{eventID}
type RespState struct {
	// A list of events giving the state of the room before the request event.
	StateEvents []Event `json:"pdus"`
	// A list of events needed to authenticate the state events.
	AuthEvents []Event `json:"auth_chain"`
}

// A RespMakeJoin is the content of a response to GET /_matrix/federation/v1/make_join/{roomID}/{userID}
type RespMakeJoin struct {
	// An incomplete m.room.member event for a user on the requesting server
	// generated by the responding server.
	// See https://matrix.org/docs/spec/server_server/unstable.html#joining-rooms
	JoinEvent EventBuilder `json:"event"`
}

// A RespSendJoin is the content of a response to PUT /_matrix/federation/v1/send_join/{roomID}/{eventID}
type RespSendJoin RespState

// MarshalJSON implements json.Marshaller
func (r RespSendJoin) MarshalJSON() ([]byte, error) {
	// SendJoinResponses contain the same data as a StateResponse but are
	// formatted slightly differently on the wire:
	//  1) The "pdus" field is renamed to "state".
	//  2) The object is placed as the second element of a two element list
	//     where the first element is the constant integer 200.
	//
	//
	// So a state response of:
	//
	//		{"pdus": x, "auth_chain": y}
	//
	// Becomes:
	//
	//      [200, {"state": x, "auth_chain": y}]
	//
	// (This protocol oddity is the result of a typo in the synapse matrix
	//  server, and is preserved to maintain compatibility.)

	return json.Marshal([]interface{}{200, respSendJoinFields{
		r.StateEvents, r.AuthEvents,
	}})
}

// UnmarshalJSON implements json.Unmarshaller
func (r *RespSendJoin) UnmarshalJSON(data []byte) error {
	var tuple []rawJSON
	if err := json.Unmarshal(data, &tuple); err != nil {
		return err
	}
	if len(tuple) != 2 {
		return fmt.Errorf("gomatrixserverlib: invalid send join response, invalid length: %d != 2", len(tuple))
	}
	var fields respSendJoinFields
	if err := json.Unmarshal(tuple[1], &fields); err != nil {
		return err
	}
	r.StateEvents = fields.StateEvents
	r.AuthEvents = fields.AuthEvents
	return nil
}

type respSendJoinFields struct {
	StateEvents []Event `json:"state"`
	AuthEvents  []Event `json:"auth_chain"`
}

// A RespDirectory is the content of a response to GET  /_matrix/federation/v1/query/directory
// This is returned when looking up a room alias from a remote server.
// See https://matrix.org/docs/spec/server_server/unstable.html#directory
type RespDirectory struct {
	// The matrix room ID the room alias corresponds to.
	RoomID string `json:"room_id"`
	// A list of matrix servers that the directory server thinks could be used
	// to join the room. The joining server may need to try multiple servers
	// before it finds one that it can use to join the room.
	Servers []ServerName `json:"servers"`
}

// Check that a response to /state is valid.
func (r RespState) Check(keyRing KeyRing) error {
	var allEvents []Event
	for _, event := range r.AuthEvents {
		if event.StateKey() == nil {
			return fmt.Errorf("gomatrixserverlib: event %q does not have a state key", event.EventID())
		}
		allEvents = append(allEvents, event)
	}

	stateTuples := map[StateKeyTuple]bool{}
	for _, event := range r.StateEvents {
		if event.StateKey() == nil {
			return fmt.Errorf("gomatrixserverlib: event %q does not have a state key", event.EventID())
		}
		if stateTuples[StateKeyTuple{event.Type(), *event.StateKey()}] {
			return fmt.Errorf(
				"gomatrixserverlib: duplicate state key tuple (%q, %q)",
				event.Type(), *event.StateKey(),
			)
		}
		allEvents = append(allEvents, event)
	}

	// Check if the events pass signature checks.
	if err := VerifyEventSignatures(allEvents, keyRing); err != nil {
		return nil
	}

	eventsByID := map[string]*Event{}
	// Collect a map of event reference to event
	for i := range allEvents {
		eventsByID[allEvents[i].EventID()] = &allEvents[i]
	}

	// Check whether the events are allowed by the auth rules.
	for _, event := range allEvents {
		if err := checkAllowedByAuthEvents(event, eventsByID); err != nil {
			return err
		}
	}

	// The checks pass.
	return nil
}

// Check that a reponse to /send_join is valid.
func (r RespSendJoin) Check(keyRing KeyRing, joinEvent Event) error {
	// First check that the state is valid.
	if err := RespState(r).Check(keyRing); err != nil {
		return err
	}

	stateEventsByID := map[string]*Event{}
	authEvents := NewAuthEvents(nil)
	for i, event := range r.StateEvents {
		stateEventsByID[event.EventID()] = &r.StateEvents[i]
		if err := authEvents.AddEvent(&r.StateEvents[i]); err != nil {
			return err
		}
	}

	// Now check that the join event is valid against its auth events.
	if err := checkAllowedByAuthEvents(joinEvent, stateEventsByID); err != nil {
		return err
	}

	// Now check that the join event is valid against the supplied state.
	if err := Allowed(joinEvent, &authEvents); err != nil {
		return fmt.Errorf(
			"gomatrixserverlib: event with ID %q is not allowed by the supplied state: %s",
			joinEvent.EventID(), err.Error(),
		)

	}

	// The checks pass
	return nil
}

func checkAllowedByAuthEvents(event Event, eventsByID map[string]*Event) error {
	authEvents := NewAuthEvents(nil)
	for _, authRef := range event.AuthEvents() {
		authEvent := eventsByID[authRef.EventID]
		if authEvent == nil {
			return fmt.Errorf(
				"gomatrixserverlib: missing auth event with ID %q for event %q",
				authRef.EventID, event.EventID(),
			)
		}
		if err := authEvents.AddEvent(authEvent); err != nil {
			return err
		}
	}
	if err := Allowed(event, &authEvents); err != nil {
		return fmt.Errorf(
			"gomatrixserverlib: event with ID %q is not allowed by its auth_events: %s",
			event.EventID(), err.Error(),
		)
	}
	return nil
}
