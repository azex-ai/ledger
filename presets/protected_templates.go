package presets

// Template codes this package ships that must never be reachable through a
// generic, caller-supplies-the-code endpoint (POST /journals/template).
//
// The four deposit-confirmation codes are posted exclusively by a
// deployment's verified-deposit orchestration via
// PostAuthorized/ExecuteTemplate (service/onchain.go's confirmed-transition
// handling, BuildDepositTolerancePlan / ExecuteDepositTolerancePlan). Each one
// lets a caller who can only name a code post a journal indistinguishable
// from a real verified deposit: DepositConfirmTemplateCode and
// DepositConfirmPendingTemplateCode both debit main_wallet (spendable funds)
// directly; DepositReleasePendingCode and DepositRecordOverageTemplateCode
// both move funds into custodial/suspense without any corresponding on-chain
// event. DevCreditTemplateCode is the fifth and the starkest: its own doc
// comment (devcredit.go) says it mints spendable holder balance with no
// custodied asset behind it, and installing it is a deliberate,
// ENV-independent decision -- the dedicated POST /dev/credits endpoint's
// three gates (admin scope, ENV=dev, DevCreditEnabled) are worth nothing if
// the same journal can be posted by naming the template code here.
//
// These values duplicate the literal Code fields already declared in
// templates.go / pending.go / devcredit.go rather than replacing them --
// deliberately, to keep this file a pure addition with no risk of shifting
// the behavior of any existing preset install.
const (
	DepositConfirmTemplateCode        = "deposit_confirm"
	DepositConfirmPendingTemplateCode = "deposit_confirm_pending"
	DepositReleasePendingTemplateCode = "deposit_release_pending"
	DepositRecordOverageTemplateCode  = "deposit_record_overage"
)

// ProtectedTemplateCodes returns the entry-template codes above as a slice.
// server.Config.ProtectedTemplateCodes defaults to this set: a deployment
// that installs any of this package's deposit bundles (InstallDefaultTemplatePresets,
// DepositBundle, InstallPendingBundle, ...) or the developer-credit bundle
// gets POST /journals/template refusing these codes without having to
// enumerate them itself -- see server.Config.AllowGenericTemplatePost to opt a
// code back in, and server.Config.ProtectedTemplateCodes to add
// deployment-specific ones on top.
//
// This is deliberately a hardcoded name list and NOT the primary defense.
// The primary defense is structural and lives at the endpoint: any template
// with a line on an is_system classification is refused, whether this package
// shipped it or the deployment defined it (server's
// rejectSystemClassificationTemplate, docs/INVARIANTS.md I-38). A hand-kept
// list can only ever cover what somebody remembered to name -- dev_credit was
// missing from this one for as long as it existed (D-C1, 2026-09-02 audit).
// The list survives that finding rather than being replaced by the derived
// rule because it answers before the template table is read, and therefore
// also when the table cannot be read or its rows do not exist yet; adding a
// code here is cheap belt-and-braces, relying on it alone is the bug.
//
// Returns a fresh slice on every call so callers can't mutate the package's
// notion of the default set through an aliased backing array.
func ProtectedTemplateCodes() []string {
	return []string{
		DepositConfirmTemplateCode,
		DepositConfirmPendingTemplateCode,
		DepositReleasePendingTemplateCode,
		DepositRecordOverageTemplateCode,
		DevCreditTemplateCode,
	}
}
