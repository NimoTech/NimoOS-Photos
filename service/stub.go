// Package service provides the core business logic for NimoOS-Photos.
// This file is a stub; full implementation is added in subsequent tasks.
package service

import (
	"context"

	"github.com/NimoTech/NimoOS-Photos/pkg/config"
)

// Service is the main service container for NimoOS-Photos.
type Service struct {
	cfg *config.Config
}

// NewService creates a new Service instance.
func NewService(cfg *config.Config) *Service {
	return &Service{cfg: cfg}
}

// Watcher returns a stub watcher that implements Start.
func (s *Service) Watcher() *stubRunner {
	return &stubRunner{}
}

// Indexer returns a stub indexer that implements Start.
func (s *Service) Indexer() *stubRunner {
	return &stubRunner{}
}

// stubRunner is a placeholder for background workers not yet implemented.
type stubRunner struct{}

// Start is a no-op placeholder.
func (r *stubRunner) Start(_ context.Context) {}
