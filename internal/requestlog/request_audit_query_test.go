package requestlog

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"gpt-load/internal/storage/models"
)

func TestRequestAuditLogFilters(t *testing.T) {
	testRequestAuditLogFilters(t, openRequestLogQueryDB(t))
}

func testRequestAuditLogFilters(t *testing.T, db *gorm.DB) {
	t.Helper()
	now := time.Now()
	model := fmt.Sprintf("guardrail-filters-%d", now.UnixNano())
	fixtures := []struct {
		name, audit string
	}{
		{"unreviewed", ""},
		{"unreviewed_json", "null"},
		{"empty_findings", `{"status":"passed","findings":null,"calls":[]}`},
		{"passed", `{"status":"passed","findings":[],"calls":[]}`},
		{"warned", `{"status":"warned","findings":[{"rule_id":"removed_rule","name":"Privacy_100%! 个人隐私","action":"warn"}],"calls":[]}`},
		{"blocked", `{"status":"blocked","findings":[{"rule_id":"credential_leakage","name":"Credential leakage","action":"block"}],"calls":[]}`},
		{"incomplete", `{"status":"incomplete","reason":"timeout","findings":[{"rule_id":"personal_data","name":"Private data","action":"warn"}],"calls":[]}`},
		{"other_key", `{"status":"warned","findings":[{"rule_id":"personal_data","name":"Private data","action":"warn"}],"calls":[]}`},
		{"upstream_error", `{"status":"warned","findings":[{"rule_id":"personal_data","name":"Private data","action":"warn"}],"calls":[]}`},
		{"sampled", `{"status":"incomplete","reason":"content_truncated","findings":[],"calls":[]}`},
	}
	ids := map[string]string{}
	for index, fixture := range fixtures {
		id := fmt.Sprintf("00000000-0000-4000-8000-%012x", (uint64(now.UnixNano())&0xffffffff0000)+uint64(index))
		ids[fixture.name] = id
		row := aggregationRow(id, now.Add(time.Duration(index)*time.Second), 1, model)
		row.ModelConsistency = "unknown"
		row.RequestAudit = models.JSON(fixture.audit)
		if strings.Contains(fixture.name, "blocked") || fixture.name == "incomplete" || fixture.name == "upstream_error" {
			row.Status, row.StatusCode = "error", 403
			row.ModelConsistency = "not_applicable"
		}
		if fixture.name == "other_key" {
			row.AccessKeyID = 2
		}
		if err := db.Create(&row).Error; err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		if err := db.Where("client_model = ?", model).Delete(&models.RequestLog{}).Error; err != nil {
			t.Error(err)
		}
	})
	service := newRequestLogTestService(db)
	key := uint(1)
	for _, test := range []struct {
		name  string
		query ListQuery
		want  []string
	}{
		{"warnings regardless of request outcome", ListQuery{AuditStatus: "warned"}, []string{"warned", "other_key", "upstream_error"}},
		{"blocked", ListQuery{AuditStatus: "blocked"}, []string{"blocked"}},
		{"incomplete", ListQuery{AuditStatus: "incomplete"}, []string{"incomplete", "sampled"}},
		{"allowed includes sampled but excludes warning hits", ListQuery{AuditStatus: "allowed"}, []string{"empty_findings", "passed", "sampled"}},
		{"failed excludes valid sampled reviews", ListQuery{AuditStatus: "failed"}, []string{"incomplete"}},
		{"rule keyword is case-insensitive", ListQuery{AuditRule: "CREDENTIAL"}, []string{"blocked"}},
		{"literal percent", ListQuery{AuditRule: "100%"}, []string{"warned"}},
		{"literal underscore", ListQuery{AuditRule: "_"}, []string{"warned"}},
		{"literal punctuation", ListQuery{AuditRule: "!"}, []string{"warned"}},
		{"Chinese keyword", ListQuery{AuditRule: "隐私"}, []string{"warned"}},
		{"combined filters", ListQuery{AuditStatus: "warned", AuditRule: "data", Status: "success", AccessKeyID: &key}, nil},
		{"combined filters across outcomes", ListQuery{AuditStatus: "warned", AuditRule: "data", AccessKeyID: &key}, []string{"upstream_error"}},
		{"no match", ListQuery{AuditRule: "unknown rule"}, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			test.query.ClientModel = model
			page, err := service.List(t.Context(), test.query)
			if err != nil {
				t.Fatal(err)
			}
			want := map[string]bool{}
			for _, name := range test.want {
				want[ids[name]] = true
			}
			if len(page.Items) != len(want) {
				t.Fatalf("got %d rows, want %d", len(page.Items), len(want))
			}
			for _, row := range page.Items {
				if !want[row.RequestID] {
					t.Fatalf("unexpected row %s", row.RequestID)
				}
			}
		})
	}
	query := ListQuery{ClientModel: model, AuditStatus: "warned", Limit: 1}
	for _, name := range []string{"upstream_error", "other_key", "warned"} {
		page, err := service.List(t.Context(), query)
		if err != nil || len(page.Items) != 1 || page.Items[0].RequestID != ids[name] {
			t.Fatalf("filtered cursor page for %s: %+v, %v", name, page, err)
		}
		query.Cursor = page.NextCursor
	}
	if query.Cursor != nil {
		t.Fatal("filtered pagination included unrelated rows")
	}
}
