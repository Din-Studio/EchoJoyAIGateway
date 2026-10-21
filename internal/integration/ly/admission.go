package ly

import (
	"context"
	"math/big"
	"net/http"
	"strings"

	"github.com/sirupsen/logrus"

	"gpt-load/internal/pricing"
)

// Credential identifies the Lingying key a balance check asks about; exactly
// one field is set. It is the balance request body.
type Credential struct {
	// APIKey is the access key plaintext the client presented.
	APIKey string `json:"api_key,omitempty"`
	// UserID is the user of an access token; Lingying bills their default key.
	UserID string `json:"user_id,omitempty"`
}

// AdmitInput is what admission needs from an authenticated request.
type AdmitInput struct {
	KeyName    string
	Credential Credential
	// ProjectID is the client's X-Project-Id header; empty when absent.
	ProjectID string
	// Currency is the request's frozen settlement currency; costs are reported in CNY.
	Currency pricing.CurrencyCode
}

// Admission identifies the Lingying account an admitted request is billed to.
// It is fixed at admission so a key deleted mid-request still reports.
type Admission struct {
	UserID    string
	KeyID     string
	ProjectID string
}

// Verdict is the admission outcome.
type Verdict uint8

const (
	Allow Verdict = iota
	InvalidKey
	Unusable
	Insufficient
	Unavailable
)

// Decision carries the verdict. Admission is set only when an allowed request
// is billed to Lingying; Reason is Lingying's reason for an Unusable account.
type Decision struct {
	Verdict   Verdict
	Reason    string
	Admission *Admission
}

// Admit lets every non-Lingying key through unbilled. A Lingying key is
// allowed only when its account is usable and has a positive balance; any
// failure to find out rejects the request.
func (s *Service) Admit(ctx context.Context, in AdmitInput) Decision {
	if !strings.HasPrefix(in.KeyName, keyNamePrefix) {
		return Decision{Verdict: Allow}
	}
	log := logrus.WithFields(logrus.Fields{"component": "ly_billing", "key_name": in.KeyName})
	if s.client == nil {
		log.Error("Lingying key rejected: LINGYING_BILLING_BASE_URL and LINGYING_BILLING_TOKEN are not configured")
		return Decision{Verdict: Unavailable}
	}
	if in.Currency != pricing.CurrencyCNY {
		log.WithField("currency", in.Currency).Error("Lingying key rejected: the system currency must be CNY")
		return Decision{Verdict: Unavailable}
	}
	status, account, err := s.balances.lookup(ctx, s.now(), in.Credential, s.client.balance)
	switch {
	case err != nil:
		log.WithError(err).WithField("status", status).Error("Lingying balance check failed")
		return Decision{Verdict: Unavailable}
	case allows(status, account):
		return Decision{Verdict: Allow, Admission: &Admission{
			UserID: account.UserID, KeyID: account.KeyID, ProjectID: in.ProjectID,
		}}
	case status == http.StatusNotFound:
		return Decision{Verdict: InvalidKey}
	case status != http.StatusOK || account.UserID == "":
		log.WithField("status", status).Error("Lingying balance check returned an unusable response")
		return Decision{Verdict: Unavailable}
	case !account.Usable:
		return Decision{Verdict: Unusable, Reason: account.Reason}
	default:
		return Decision{Verdict: Insufficient}
	}
}

// allows reports whether a balance check lets a request through: the account
// is known, usable and has a positive balance. It is the only result the
// balance cache keeps.
func allows(status int, account balance) bool {
	return status == http.StatusOK && account.UserID != "" && account.Usable && positive(account.Available)
}

// positive reports whether a decimal amount is greater than zero. An
// unparsable amount is not.
func positive(amount string) bool {
	value, ok := new(big.Rat).SetString(amount)
	return ok && value.Sign() > 0
}
