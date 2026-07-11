package server

import "github.com/go-chi/chi/v5"

// setupRoutes registers every endpoint, grouped by the API key scope it
// requires (read < write < admin — see middleware_auth.go):
//
//   - read:  the query surface, plus POST endpoints that are semantically
//     reads (batch balance lookup, template preview)
//   - write: business writes — journals, reversals, reservations, bookings
//   - admin: configuration + control plane — metadata mutations, account
//     policies, reconciliation triggers, period close
//
// Probes and inbound webhooks carry no scope: probes are unauthenticated,
// webhooks authenticate via the channel adapter's own signature scheme.
func (s *Server) setupRoutes() {
	s.router.Route("/api/v1", func(r chi.Router) {
		// Probes (unauthenticated) + webhooks (channel HMAC).
		r.Get("/system/health", s.handleHealth)
		r.Get("/system/ready", s.handleReady)
		r.Post("/webhooks/{channel}", s.handleWebhookCallback)

		// ---- Scope: read ----
		r.Group(func(r chi.Router) {
			r.Use(s.requireScope(ScopeRead))

			r.Get("/system/balances", s.handleSystemBalances)

			r.Get("/journals/{uid}", s.handleGetJournal)
			r.Get("/journals", s.handleListJournals)
			r.Get("/entries", s.handleListEntries)

			r.Get("/balances/{holder}", s.handleGetBalances)
			r.Get("/balances/{holder}/{currency}", s.handleGetBalanceByCurrency)
			r.Get("/balances/{holder}/{currency}/breakdown", s.handleGetBalanceBreakdown)
			r.Post("/balances/batch", s.handleBatchBalances) // POST body, semantic read

			r.Get("/reservations", s.handleListReservations)
			r.Get("/bookings/{uid}", s.handleGetBooking)
			r.Get("/bookings", s.handleListBookings)

			r.Get("/events", s.handleListEvents)
			r.Get("/events/{uid}", s.handleGetEvent)

			r.Get("/classifications", s.handleListClassifications)
			r.Get("/journal-types", s.handleListJournalTypes)
			r.Get("/templates", s.handleListTemplates)
			r.Post("/templates/{code}/preview", s.handlePreviewTemplate) // render only, no mutation
			r.Get("/currencies", s.handleListCurrencies)

			r.Get("/accounts/{holder}/policies", s.handleListAccountPolicies)
			r.Get("/snapshots", s.handleListSnapshots)

			// Crypto deposit add-on (query side). 404s via
			// bizcode.FeatureNotEnabled until SetDepositAddressProvider is
			// called (see handler_onchain.go).
			r.Get("/holders/{holder}/deposit-address", s.handleGetDepositAddress)

			// Crypto deposit add-on (human-review queue). 404s via
			// bizcode.FeatureNotEnabled until SetDepositReviewer is called
			// (see handler_deposit_reviews.go).
			r.Get("/deposits/reviews", s.handleListDepositReviews)

			r.Get("/audit/journals", s.handleListAuditJournals)
			r.Get("/audit/bookings/{uid}/trace", s.handleTraceBooking)
			r.Get("/audit/journals/{uid}/reversals", s.handleListReversals)

			r.Get("/platform/balances", s.handleGetPlatformBalances)
			r.Get("/platform/solvency", s.handleGetSolvency)
			r.Get("/balances/trends", s.handleGetBalanceTrends)

			r.Get("/periods/closes", s.handleListPeriodCloses)
			r.Get("/reports/trial-balance", s.handleGetTrialBalance)
		})

		// ---- Holder wallet surface (holder-token auth, not API keys) ----
		// Registered unconditionally; every handler 404s via
		// requireHolderSurface until SetHolderSurface configures it.
		r.Group(func(r chi.Router) {
			r.Use(s.holderTokenAuth)
			r.Get("/holder/balances", s.withHolderSurface((*holderSurface).handleHolderBalances))
			r.Get("/holder/transactions", s.withHolderSurface((*holderSurface).handleHolderTransactions))
			r.Get("/holder/holds", s.withHolderSurface((*holderSurface).handleHolderHolds))
		})

		// ---- Scope: write ----
		r.Group(func(r chi.Router) {
			r.Use(s.requireScope(ScopeWrite))

			r.Post("/holder-tokens", s.withHolderSurface((*holderSurface).handleMintHolderToken))

			r.Post("/journals", s.handlePostJournal)
			r.Post("/journals/template", s.handlePostTemplate)
			r.Post("/journals/deposit-tolerance", s.handlePostDepositTolerance)
			r.Post("/journals/{uid}/reverse", s.handleReverseJournal)
			r.Post("/journals/{uid}/reverse-partial", s.handleReverseJournalFraction)

			r.Post("/reservations", s.handleCreateReservation)
			r.Post("/reservations/{uid}/settle", s.handleSettleReservation)
			r.Post("/reservations/{uid}/settle-partial", s.handleSettlePartialReservation)
			r.Post("/reservations/{uid}/finalize", s.handleFinalizeReservationSettlement)
			r.Post("/reservations/{uid}/release", s.handleReleaseReservation)

			r.Post("/bookings", s.handleCreateBooking)
			r.Post("/bookings/{uid}/transition", s.handleTransition)

			// Crypto deposit add-on (issuance side) — idempotent, safe to
			// call repeatedly for the same holder.
			r.Post("/holders/{holder}/deposit-address", s.handleEnsureDepositAddress)

			// Crypto deposit add-on (human-review resolution). Idempotent —
			// see DepositReviewer's ApproveReview/RejectReview contracts.
			r.Post("/deposits/{uid}/review/approve", s.handleApproveDepositReview)
			r.Post("/deposits/{uid}/review/reject", s.handleRejectDepositReview)
		})

		// ---- Scope: admin ----
		r.Group(func(r chi.Router) {
			r.Use(s.requireScope(ScopeAdmin))

			r.Post("/classifications", s.handleCreateClassification)
			r.Post("/classifications/{uid}/deactivate", s.handleDeactivateClassification)
			r.Post("/journal-types", s.handleCreateJournalType)
			r.Post("/journal-types/{uid}/deactivate", s.handleDeactivateJournalType)
			r.Post("/templates", s.handleCreateTemplate)
			r.Post("/templates/{uid}/deactivate", s.handleDeactivateTemplate)
			r.Post("/currencies", s.handleCreateCurrency)
			r.Post("/currencies/{uid}/deactivate", s.handleDeactivateCurrency)

			r.Put("/accounts/{holder}/policy", s.handleSetAccountPolicy)

			r.Post("/reconcile", s.handleReconcileGlobal)
			r.Post("/reconcile/account", s.handleReconcileAccount)
			r.Post("/reconcile/full", s.handleReconcileFull)

			r.Post("/periods/close", s.handleClosePeriod)
		})
	})
}
