//go:build wakemodel

package voice

import _ "embed"

// bundledWakeModel is the "Hey Maverick" model trained for the default
// persona's earlier name, built in because this program was made with -tags
// wakemodel. Its training data isn't cleared for commercial use, so release
// builds leave it out (wakemodel.go). It doesn't hear "Hey Mirrin".
//
//go:embed assets/hey_maverick.onnx
var bundledWakeModel []byte
