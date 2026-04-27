// Package presets provides out-of-the-box classification, journal-type,
// and entry-template bundles that callers can install into the ledger to
// get standard deposit and withdrawal flows without designing them from
// scratch.
//
// Public surface:
//
//   - DepositLifecycle, WithdrawalLifecycle: lifecycle state machines
//     suitable for attaching to a Classification.
//   - DefaultTemplateClassifications, DefaultTemplateJournalTypes,
//     DefaultTemplatePresets: the metadata records installed by the
//     defaults below.
//   - InstallDefaultTemplatePresets: idempotently installs the defaults.
//   - InstallTemplatePresets: installs an arbitrary preset bundle.
//   - BuildDepositTolerancePlan / ExecuteDepositTolerancePlan: deposit
//     tolerance settlement (auto-confirm, auto-release-shortfall, or
//     manual review) executed as one atomic template batch.
package presets
