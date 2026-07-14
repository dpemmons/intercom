package main

import (
	"strings"
	"testing"
	"time"

	"github.com/dpemmons/intercom/internal/broker"
)

func TestBrokerIdleAfterMapsUserFacingZeroToDisabled(t *testing.T) {
	t.Parallel()
	if got := brokerIdleAfter(0); got != broker.IdleExitDisabled {
		t.Fatalf("brokerIdleAfter(0) = %s, want disabled sentinel %s", got, broker.IdleExitDisabled)
	}
	if got := brokerIdleAfter(time.Minute); got != time.Minute {
		t.Fatalf("brokerIdleAfter(1m) = %s, want 1m", got)
	}
}

func TestResolveBrokerIdleAfterEnvironmentAndFlagPrecedence(t *testing.T) {
	t.Setenv("INTERCOM_IDLE_EXIT", "2m")
	cmd := newBrokerCmd()
	got, err := resolveBrokerIdleAfter(cmd, broker.DefaultIdleAfter)
	if err != nil {
		t.Fatal(err)
	}
	if got != 2*time.Minute {
		t.Fatalf("environment idle after = %s, want 2m", got)
	}

	if err := cmd.Flags().Set("idle-after", "3m"); err != nil {
		t.Fatal(err)
	}
	got, err = resolveBrokerIdleAfter(cmd, 3*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if got != 3*time.Minute {
		t.Fatalf("flag idle after = %s, want 3m", got)
	}
}

func TestResolveBrokerIdleAfterEnvironmentZeroDisables(t *testing.T) {
	t.Setenv("INTERCOM_IDLE_EXIT", "0")
	got, err := resolveBrokerIdleAfter(newBrokerCmd(), broker.DefaultIdleAfter)
	if err != nil {
		t.Fatal(err)
	}
	if got != broker.IdleExitDisabled {
		t.Fatalf("environment zero = %s, want disabled sentinel %s", got, broker.IdleExitDisabled)
	}
}

func TestResolveBrokerIdleAfterRejectsInvalidEnvironment(t *testing.T) {
	t.Setenv("INTERCOM_IDLE_EXIT", "eventually")
	_, err := resolveBrokerIdleAfter(newBrokerCmd(), broker.DefaultIdleAfter)
	if err == nil || !strings.Contains(err.Error(), "INTERCOM_IDLE_EXIT") {
		t.Fatalf("error = %v, want invalid INTERCOM_IDLE_EXIT", err)
	}
}

func TestResolveBrokerIdleAfterRejectsNegativeDuration(t *testing.T) {
	t.Setenv("INTERCOM_IDLE_EXIT", "-1s")
	_, err := resolveBrokerIdleAfter(newBrokerCmd(), broker.DefaultIdleAfter)
	if err == nil || !strings.Contains(err.Error(), "non-negative") {
		t.Fatalf("error = %v, want non-negative duration error", err)
	}
}
