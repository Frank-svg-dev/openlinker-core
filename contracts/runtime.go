package contracts

import _ "embed"

// RuntimeV2 contains the canonical runtime v2 contract shared by Core,
// Agent Node, and the public SDKs.
//
//go:embed core-runtime.v2.json
var RuntimeV2 []byte
