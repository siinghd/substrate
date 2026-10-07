package main

import "github.com/substrate-sh/substrate/internal/reconcile"

type liveResolver struct{ sup *reconcile.Supervisor }

func (l liveResolver) LiveIDs() []string {
	return l.sup.LiveSandboxes()
}
