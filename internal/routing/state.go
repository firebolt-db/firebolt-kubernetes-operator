// Copyright 2026 Firebolt Analytics
// SPDX-License-Identifier: Apache-2.0

// Package routing defines the durable gateway registration and withdrawal
// protocol. Its mutations must be persisted with a Kubernetes resource-version
// compare-and-swap before they are published to agents or acted upon.
package routing

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
)

// DataKey names the ConfigMap entry containing the complete coordination record.
const DataKey = "routing.json"

// State is owned by the operator. Agents may only observe it. Revision orders
// registrations and route changes in one durable record across operator restarts.
type State struct {
	Revision    uint64                `json:"revision"`
	InstanceUID string                `json:"instanceUID"`
	Closed      bool                  `json:"closed"`
	Routes      map[string]Route      `json:"routes"`
	Sessions    map[string]Session    `json:"sessions"`
	Retirements map[string]Retirement `json:"retirements"`
}

// Route is an immutable engine destination within one admission epoch.
type Route struct {
	EngineUID  string `json:"engineUID"`
	Epoch      uint64 `json:"epoch"`
	Generation int    `json:"generation"`
	Authority  string `json:"authority"`
	Enabled    bool   `json:"enabled"`
}

// Key identifies request counts and retirement obligations for this epoch.
func (r Route) Key() string { return r.EngineUID + "/" + strconv.FormatUint(r.Epoch, 10) }

// Session identifies one agent process registered for a gateway Pod.
type Session struct {
	ID string `json:"id"`
}

// Retirement captures every session registered before a route is closed. A
// later registration cannot acquire the closed route, and cannot replace a
// captured session's acknowledgement.
type Retirement struct {
	Route            Route             `json:"route"`
	RequiredRevision uint64            `json:"requiredRevision"`
	Holders          map[string]string `json:"holders"`
}

// Count records permits that are active or whose completion is uncertain.
type Count struct {
	Active  uint64 `json:"active"`
	Unknown uint64 `json:"unknown"`
}

// Report is a single atomic snapshot of an agent's applied state and permits.
// Missing keys mean zero only after that same session has applied the fence.
type Report struct {
	PodUID          string           `json:"podUID"`
	SessionID       string           `json:"sessionID"`
	AppliedRevision uint64           `json:"appliedRevision"`
	Registered      bool             `json:"registered"`
	Outstanding     map[string]Count `json:"outstanding"`
}

// NewState constructs an empty coordination record with admission disabled.
func NewState(instanceUID string) State {
	return State{Revision: 1, InstanceUID: instanceUID, Routes: map[string]Route{},
		Sessions: map[string]Session{}, Retirements: map[string]Retirement{}}
}

// ConfigMapName does not embed an arbitrary-length Instance name in a DNS label.
func ConfigMapName(instanceName string) string {
	sum := sha256.Sum256([]byte(instanceName))
	return "gateway-routing-" + hex.EncodeToString(sum[:16])
}

// GenerationServiceName is immutable across routing epochs for one generation.
// Engine UID prevents a same-name engine recreation from reusing a destination.
func GenerationServiceName(engineUID string, generation int) string {
	sum := sha256.Sum256([]byte(engineUID))
	return "engine-route-" + hex.EncodeToString(sum[:12]) + "-g" + strconv.Itoa(generation)
}

// Decode rejects absent or inconsistent coordination records.
func Decode(data []byte) (State, error) {
	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return State{}, fmt.Errorf("decode routing state: %w", err)
	}
	if err := state.Validate(); err != nil {
		return State{}, err
	}
	return state, nil
}

// Encode validates a coordination record before serialization.
func Encode(state State) ([]byte, error) {
	if err := state.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(state)
}

// Validate checks the internal ordering and identity constraints of a record.
func (s State) Validate() error {
	if s.Revision == 0 || s.InstanceUID == "" || s.Routes == nil || s.Sessions == nil || s.Retirements == nil {
		return errors.New("routing state requires revision, instance UID, routes, sessions and retirements")
	}
	for name, route := range s.Routes {
		if name == "" || !validRoute(route, s.Revision) {
			return fmt.Errorf("invalid route %q", name)
		}
		if s.Closed && route.Enabled {
			return errors.New("closed instance must not contain enabled routes")
		}
		if _, closed := s.Retirements[route.Key()]; closed {
			return fmt.Errorf("current route %q is retired", name)
		}
	}
	for podUID, session := range s.Sessions {
		if podUID == "" || session.ID == "" {
			return errors.New("invalid routing session")
		}
	}
	for key, retirement := range s.Retirements {
		if !validRoute(retirement.Route, s.Revision) || !retirement.Route.Enabled || key != retirement.Route.Key() ||
			retirement.RequiredRevision <= retirement.Route.Epoch || retirement.RequiredRevision > s.Revision || retirement.Holders == nil {
			return fmt.Errorf("invalid retirement %q", key)
		}
		for podUID, sessionID := range retirement.Holders {
			if podUID == "" || sessionID == "" {
				return fmt.Errorf("invalid holder in retirement %q", key)
			}
		}
	}
	return nil
}

func validRoute(route Route, revision uint64) bool {
	return route.EngineUID != "" && route.Epoch > 0 && route.Epoch <= revision && route.Generation >= 0 && route.Authority != ""
}

func nextRevision(s *State) (uint64, error) {
	if err := s.Validate(); err != nil {
		return 0, err
	}
	if s.Revision == math.MaxUint64 {
		return 0, errors.New("routing revision exhausted")
	}
	return s.Revision + 1, nil
}

// RegisterSession refuses an agent restart in the same Pod: its predecessor may
// have lost request accounting while Envoy is still running. Recovery requires
// positively stopping that Envoy and creating a new gateway Pod.
func RegisterSession(s *State, podUID, sessionID string) (bool, error) {
	revision, err := nextRevision(s)
	if err != nil {
		return false, err
	}
	if podUID == "" || sessionID == "" {
		return false, errors.New("registration requires pod UID and session ID")
	}
	if current, exists := s.Sessions[podUID]; exists {
		if current.ID != sessionID {
			return false, fmt.Errorf("gateway pod %q has a different registered session", podUID)
		}
		return false, nil
	}
	s.Sessions[podUID] = Session{ID: sessionID}
	s.Revision = revision
	return true, nil
}

// SetRoute assigns a fresh epoch and snapshots holders of the previous enabled
// route in the same mutation. Callers supply the destination, not the epoch.
func SetRoute(s *State, engineName string, route Route) (bool, error) {
	revision, err := nextRevision(s)
	if err != nil {
		return false, err
	}
	route.Epoch = revision
	if s.Closed && route.Enabled {
		return false, errors.New("instance routing is permanently closed")
	}
	if engineName == "" || !validRoute(route, revision) {
		return false, errors.New("route requires engine name, UID, nonnegative generation and authority")
	}
	old, exists := s.Routes[engineName]
	if exists && old.EngineUID == route.EngineUID && (route.Generation < old.Generation ||
		(!old.Enabled && route.Enabled && route.Generation == old.Generation)) {
		return false, errors.New("route must not reactivate a withdrawn or older engine generation")
	}
	if exists && old.EngineUID == route.EngineUID && old.Generation == route.Generation &&
		old.Authority == route.Authority && old.Enabled == route.Enabled {
		return false, nil
	}
	if exists && old.Enabled {
		holders := make(map[string]string, len(s.Sessions))
		for podUID, session := range s.Sessions {
			holders[podUID] = session.ID
		}
		s.Retirements[old.Key()] = Retirement{Route: old, RequiredRevision: revision, Holders: holders}
	}
	s.Routes[engineName] = route
	s.Revision = revision
	return true, nil
}

// Withdraw disables new admission while keeping an explicit route record. The
// closed epoch remains in Retirements until its holders are accounted for.
func Withdraw(s *State, engineName string) (bool, error) {
	if err := s.Validate(); err != nil {
		return false, err
	}
	route, exists := s.Routes[engineName]
	if !exists {
		return false, nil
	}
	route.Enabled = false
	return SetRoute(s, engineName, route)
}

// CloseInstance permanently prevents new route publication and withdraws every
// enabled route in one durable mutation. The sticky fence serializes teardown
// with engine controllers whose earlier Instance read preceded deletion.
func CloseInstance(s *State) (bool, error) {
	revision, err := nextRevision(s)
	if err != nil {
		return false, err
	}
	if s.Closed {
		return false, nil
	}
	for name, route := range s.Routes {
		if !route.Enabled {
			continue
		}
		holders := make(map[string]string, len(s.Sessions))
		for podUID, session := range s.Sessions {
			holders[podUID] = session.ID
		}
		s.Retirements[route.Key()] = Retirement{Route: route, RequiredRevision: revision, Holders: holders}
		route.Enabled = false
		route.Epoch = revision
		s.Routes[name] = route
	}
	s.Closed = true
	s.Revision = revision
	return true, nil
}

// CanRetire accepts either a matching session's post-fence zero report or
// positive evidence that its Envoy process stopped. Pod absence, unready status,
// elapsed time and agent unreachability are not positive stop evidence.
func CanRetire(retirement Retirement, reports map[string]Report, stopped map[string]bool) bool {
	if retirement.Holders == nil || retirement.RequiredRevision <= retirement.Route.Epoch ||
		!retirement.Route.Enabled || !validRoute(retirement.Route, retirement.RequiredRevision) {
		return false
	}
	for podUID, sessionID := range retirement.Holders {
		if podUID == "" || sessionID == "" {
			return false
		}
		if stopped[podUID] {
			continue
		}
		report, exists := reports[podUID]
		if !exists || report.Outstanding == nil || report.PodUID != podUID || report.SessionID != sessionID || !report.Registered ||
			report.AppliedRevision < retirement.RequiredRevision {
			return false
		}
		count := report.Outstanding[retirement.Route.Key()]
		if count.Active != 0 || count.Unknown != 0 {
			return false
		}
	}
	return true
}

// ForgetStoppedSession durably removes a positively stopped process from future
// snapshots and existing obligations. Removing holders here preserves stop
// evidence even after Kubernetes garbage-collects the terminated Pod object.
func ForgetStoppedSession(s *State, podUID string, positivelyStopped bool) (bool, error) {
	revision, err := nextRevision(s)
	if err != nil {
		return false, err
	}
	if podUID == "" || !positivelyStopped {
		return false, errors.New("forgetting a gateway requires positive process termination evidence")
	}
	_, changed := s.Sessions[podUID]
	delete(s.Sessions, podUID)
	for key, retirement := range s.Retirements {
		if _, exists := retirement.Holders[podUID]; exists {
			delete(retirement.Holders, podUID)
			s.Retirements[key] = retirement
			changed = true
		}
	}
	if changed {
		s.Revision = revision
	}
	return changed, nil
}

// CompleteRetirement removes a satisfied obligation. The caller must finish
// retiring its engine generation before calling this: removal is not itself an
// authorization token for a later shutdown attempt.
func CompleteRetirement(s *State, key string, reports map[string]Report, stopped map[string]bool) (bool, error) {
	revision, err := nextRevision(s)
	if err != nil {
		return false, err
	}
	retirement, exists := s.Retirements[key]
	if !exists {
		return false, nil
	}
	if !CanRetire(retirement, reports, stopped) {
		return false, fmt.Errorf("retirement %q still has unresolved holders", key)
	}
	delete(s.Retirements, key)
	s.Revision = revision
	return true, nil
}

// ForgetEngine removes a disabled route only after all its retirements have
// completed. The caller must also verify that the Engine UID no longer exists;
// stale controller reads must not recreate a forgotten engine's route.
func ForgetEngine(s *State, engineName string) (bool, error) {
	revision, err := nextRevision(s)
	if err != nil {
		return false, err
	}
	route, exists := s.Routes[engineName]
	if !exists {
		return false, nil
	}
	if route.Enabled {
		return false, errors.New("engine route must be withdrawn before forgetting it")
	}
	for _, retirement := range s.Retirements {
		if retirement.Route.EngineUID == route.EngineUID {
			return false, errors.New("engine route has unresolved retirements")
		}
	}
	delete(s.Routes, engineName)
	s.Revision = revision
	return true, nil
}
