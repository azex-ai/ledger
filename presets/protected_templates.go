package presets

// Deposit-confirmation template codes that this package's own deposit
// lifecycle posts exclusively via PostAuthorized/ExecuteTemplate from a
// deployment's verified-deposit orchestration (service/onchain.go's
// confirmed-transition handling, BuildDepositTolerancePlan /
// ExecuteDepositTolerancePlan) -- never through a generic caller-supplied
// template_code endpoint. Each one lets a caller who can only name a code
// post a journal indistinguishable from a real verified deposit:
// DepositConfirmTemplateCode and DepositConfirmPendingTemplateCode both
// debit main_wallet (spendable funds) directly; DepositReleasePendingCode
// and DepositRecordOverageTemplateCode both move funds into custodial/
// suspense without any corresponding on-chain event.
//
// These values duplicate the literal Code fields already declared in
// templates.go / pending.go rather than replacing them -- deliberately, to
// keep this file a pure addition with no risk of shifting the behavior of
// any existing preset install.
const (
	DepositConfirmTemplateCode        = "deposit_confirm"
	DepositConfirmPendingTemplateCode = "deposit_confirm_pending"
	DepositReleasePendingTemplateCode = "deposit_release_pending"
	DepositRecordOverageTemplateCode  = "deposit_record_overage"
)

// ProtectedTemplateCodes returns the entry-template codes above as a slice.
// server.Config.ProtectedTemplateCodes defaults to this set: a deployment
// that installs any of this package's deposit bundles (InstallDefaultTemplatePresets,
// DepositBundle, InstallPendingBundle, ...) gets POST /journals/template
// refusing these codes without having to enumerate them itself -- see
// server.Config.AllowGenericTemplatePost to opt a code back in, and
// server.Config.ProtectedTemplateCodes to add deployment-specific ones on
// top.
//
// Returns a fresh slice on every call so callers can't mutate the package's
// notion of the default set through an aliased backing array.
func ProtectedTemplateCodes() []string {
	return []string{
		DepositConfirmTemplateCode,
		DepositConfirmPendingTemplateCode,
		DepositReleasePendingTemplateCode,
		DepositRecordOverageTemplateCode,
	}
}
