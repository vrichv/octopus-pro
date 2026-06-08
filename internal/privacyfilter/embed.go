package privacyfilter

import _ "embed"

//go:embed gitleaks.toml
var GitleaksRules []byte
