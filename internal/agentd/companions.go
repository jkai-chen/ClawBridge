package agentd

import "regexp"

// providerCompanions are the second hosts some providers' APIs need besides
// their own (§5.3, Q3): the token endpoint where a cloud credential is
// exchanged for a short-lived token. The table is compiled in, like the
// container spec: the server names an owner's provider host, never these,
// and an agent may reach a companion only beside its own provider.
//
// None of these providers is offered by the agent yet (it takes key-based
// APIs only); the table is ready for when they gain an explicit-credential
// path.
var providerCompanions = []struct {
	provider  *regexp.Regexp
	companion string
}{
	// Vertex AI: a service-account key is exchanged at Google's OAuth endpoint.
	{regexp.MustCompile(`^(?:[a-z0-9-]+-)?aiplatform\.googleapis\.com:443$`), "oauth2.googleapis.com:443"},
	// Bedrock: assumed roles and web identities go through the region's STS.
	{regexp.MustCompile(`^bedrock-runtime(?:-fips)?\.([a-z]{2}(?:-[a-z]+)+-[0-9])\.amazonaws\.com:443$`), "sts.$1.amazonaws.com:443"},
	// Azure OpenAI and AI services: Entra ID tokens come from Microsoft's login host.
	{regexp.MustCompile(`^[a-z0-9-]+\.(?:openai\.azure\.com|cognitiveservices\.azure\.com):443$`), "login.microsoftonline.com:443"},
}

// companionOf returns the companion host:port of provider, a normalized
// host:port, or "" when its API needs none.
func companionOf(provider string) string {
	for _, c := range providerCompanions {
		if m := c.provider.FindStringSubmatchIndex(provider); m != nil {
			return string(c.provider.ExpandString(nil, c.companion, provider, m))
		}
	}
	return ""
}
