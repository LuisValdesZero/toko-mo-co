package rules

import "testing"

func TestRedirectProvidersActionCompilesAndApplies(t *testing.T) {
	act, err := compileAction(ActionSpec{
		Type:              ActionRedirect,
		RedirectProviders: []string{"or-openai", "deepseek"},
	}, nil)
	if err != nil {
		t.Fatalf("compileAction: %v", err)
	}
	res := act.Apply(nil, &Rule{Name: "failover"})
	if res.Action != ActionRedirect {
		t.Fatalf("action = %v, want redirect", res.Action)
	}
	if len(res.RedirectProviders) != 2 || res.RedirectProviders[0] != "or-openai" || res.RedirectProviders[1] != "deepseek" {
		t.Fatalf("redirect chain = %v", res.RedirectProviders)
	}

	// A redirect with neither providers nor URL is invalid.
	if _, err := compileAction(ActionSpec{Type: ActionRedirect}, nil); err == nil {
		t.Fatal("expected error for redirect with no providers and no url")
	}

	// The built-in Provider-Failover template carries a provider chain.
	var tmpl *RuleTemplate
	for i := range BuiltinTemplates() {
		if BuiltinTemplates()[i].ID == "provider-redirect" {
			tmpl = &BuiltinTemplates()[i]
			break
		}
	}
	if tmpl == nil || len(tmpl.Action.RedirectProviders) == 0 {
		t.Fatalf("provider-redirect template should seed a redirect_providers chain, got %+v", tmpl)
	}
}
