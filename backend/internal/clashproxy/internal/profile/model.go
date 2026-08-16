package profile

import (
	"errors"
	"strings"
)

type Strategy string

const (
	StrategySelect      Strategy = "select"
	StrategyURLTest     Strategy = "url_test"
	StrategyFallback    Strategy = "fallback"
	StrategyLoadBalance Strategy = "load_balance"
)

type Status string

const (
	StatusActive   Status = "active"
	StatusDisabled Status = "disabled"
)

type Profile struct {
	ID              int64
	Name            string
	Strategy        Strategy
	TestURL         string
	IntervalSeconds int
	Status          Status
	Config          map[string]any
	Runtime         map[string]any
}

func (p Profile) ValidateActive() error {
	if p.ID <= 0 {
		return errors.New("proxy profile id is required")
	}
	if p.Status != "" && p.Status != StatusActive {
		return errors.New("proxy profile is not active")
	}
	if !IsAllowedStrategy(p.Strategy) {
		return errors.New("proxy profile strategy is not supported")
	}
	return nil
}

func IsAllowedStrategy(strategy Strategy) bool {
	switch Strategy(strings.TrimSpace(string(strategy))) {
	case "", StrategySelect, StrategyURLTest, StrategyFallback, StrategyLoadBalance:
		return true
	default:
		return false
	}
}
