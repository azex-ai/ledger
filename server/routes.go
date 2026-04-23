package server

import "github.com/go-chi/chi/v5"

func (s *Server) setupRoutes() {
	s.router.Route("/api/v1", func(r chi.Router) {
		// System
		r.Get("/system/health", s.handleHealth)
		r.Get("/system/balances", s.handleSystemBalances)

		// Journals
		r.Post("/journals", s.handlePostJournal)
		r.Post("/journals/template", s.handlePostTemplate)
		r.Post("/journals/{id}/reverse", s.handleReverseJournal)
		r.Get("/journals/{id}", s.handleGetJournal)
		r.Get("/journals", s.handleListJournals)

		// Entries
		r.Get("/entries", s.handleListEntries)

		// Balances
		r.Get("/balances/{holder}", s.handleGetBalances)
		r.Get("/balances/{holder}/{currency}", s.handleGetBalanceByCurrency)
		r.Post("/balances/batch", s.handleBatchBalances)

		// Reservations
		r.Post("/reservations", s.handleCreateReservation)
		r.Post("/reservations/{id}/settle", s.handleSettleReservation)
		r.Post("/reservations/{id}/release", s.handleReleaseReservation)
		r.Get("/reservations", s.handleListReservations)

		// Bookings (unified — replaces deposits + withdrawals)
		r.Post("/bookings", s.handleCreateBooking)
		r.Post("/bookings/{id}/transition", s.handleTransition)
		r.Get("/bookings/{id}", s.handleGetBooking)
		r.Get("/bookings", s.handleListBookings)

		// Webhooks (inbound channel callbacks)
		r.Post("/webhooks/{channel}", s.handleWebhookCallback)

		// Events (outbound)
		r.Get("/events", s.handleListEvents)
		r.Get("/events/{id}", s.handleGetEvent)

		// Metadata — Classifications
		r.Post("/classifications", s.handleCreateClassification)
		r.Post("/classifications/{id}/deactivate", s.handleDeactivateClassification)
		r.Get("/classifications", s.handleListClassifications)

		// Metadata — Journal Types
		r.Post("/journal-types", s.handleCreateJournalType)
		r.Post("/journal-types/{id}/deactivate", s.handleDeactivateJournalType)
		r.Get("/journal-types", s.handleListJournalTypes)

		// Metadata — Templates
		r.Post("/templates", s.handleCreateTemplate)
		r.Post("/templates/{id}/deactivate", s.handleDeactivateTemplate)
		r.Post("/templates/{code}/preview", s.handlePreviewTemplate)
		r.Get("/templates", s.handleListTemplates)

		// Metadata — Currencies
		r.Post("/currencies", s.handleCreateCurrency)
		r.Get("/currencies", s.handleListCurrencies)

		// Reconciliation + Snapshots
		r.Post("/reconcile", s.handleReconcileGlobal)
		r.Post("/reconcile/account", s.handleReconcileAccount)
		r.Get("/snapshots", s.handleListSnapshots)
	})
}
