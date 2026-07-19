package controller

import (
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/clavex-eu/clavex-operator/internal/eventstream"
)

// streamSource registers a Kind's lister with the event-stream Manager and
// returns a controller-runtime source that enqueues a reconcile for each CR the
// Manager pushes when a matching live Admin API event arrives. Each controller
// wires it in SetupWithManager via WatchesRawSource when an EventStream is set.
func streamSource(es *eventstream.Manager, resourceType string, lister eventstream.Lister) source.Source {
	return source.Channel(es.Register(resourceType, lister), &handler.EnqueueRequestForObject{})
}
