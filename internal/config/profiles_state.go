package config

import (
	"context"
	"fmt"
	"sync"

	coreProfile "github.com/logoscore/logos-golang-channel-core/pkg/profile"
)

// ProfilesState is an in-memory profile store that implements both the
// httpserver profilesProvider interface (Profiles) and the channel-core
// runtime.ProfileStore interface (List, Get, Put, Delete).
type ProfilesState struct {
	mu       sync.RWMutex
	profiles []coreProfile.Profile
}

func NewProfilesState(initial []coreProfile.Profile) *ProfilesState {
	cp := append([]coreProfile.Profile(nil), initial...)
	return &ProfilesState{profiles: cp}
}

func (s *ProfilesState) Profiles() []coreProfile.Profile {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]coreProfile.Profile(nil), s.profiles...)
}

func (s *ProfilesState) List(_ context.Context, _ string) ([]coreProfile.Profile, error) {
	return s.Profiles(), nil
}

func (s *ProfilesState) Get(_ context.Context, _ string, profileID int32) (coreProfile.Profile, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, p := range s.profiles {
		if p.ProfileID == profileID {
			return p, nil
		}
	}
	return coreProfile.Profile{}, fmt.Errorf("profile %d not found", profileID)
}

func (s *ProfilesState) Put(_ context.Context, _ string, p coreProfile.Profile) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, existing := range s.profiles {
		if existing.ProfileID == p.ProfileID {
			s.profiles[i] = p
			return nil
		}
	}
	s.profiles = append(s.profiles, p)
	return nil
}

func (s *ProfilesState) Delete(_ context.Context, _ string, profileID int32) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, p := range s.profiles {
		if p.ProfileID == profileID {
			s.profiles = append(s.profiles[:i], s.profiles[i+1:]...)
			return nil
		}
	}
	return fmt.Errorf("profile %d not found", profileID)
}
