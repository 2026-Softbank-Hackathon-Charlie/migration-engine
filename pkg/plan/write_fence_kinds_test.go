package plan

import (
	"strings"
	"testing"
)

func TestSourceWriteFenceRejectsDatabaseLessAndExternalEndpoints(t *testing.T) {
	for _, kind := range []string{KindNone, SourceExternal} {
		t.Run(kind, func(t *testing.T) {
			p := &Plan{Source: Endpoint{Kind: kind}, Target: Endpoint{Kind: kind}, SourceWriteFence: &SourceWriteFenceSpec{Mode: "cnpg_hba", WriterRoles: []string{"app"}}}
			p.applyDefaults()
			if err := p.Validate(); err == nil || !strings.Contains(err.Error(), "sourceWriteFence only supports CNPG") {
				t.Fatalf("unsupported endpoint accepted: %v", err)
			}
		})
	}
}
