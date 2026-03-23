//go:build tools

package mobile

// Ensure golang.org/x/mobile/bind stays in go.mod for gomobile bind.
import _ "golang.org/x/mobile/bind"
