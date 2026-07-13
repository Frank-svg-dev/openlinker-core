package contracts

import _ "embed"

// RuntimeContract contains the canonical Runtime contract shared by Core,
// Runtime Workers, and the public SDKs.
//
//go:embed core-runtime.json
var RuntimeContract []byte
