//go:build android

package client

import "sync"

var androidID struct {
	mu sync.Mutex
	id string
}

// SetAndroidMachineID sets the machine ID from Java (typically ANDROID_ID).
func SetAndroidMachineID(id string) {
	androidID.mu.Lock()
	androidID.id = id
	androidID.mu.Unlock()
}

func machineID() string {
	androidID.mu.Lock()
	id := androidID.id
	androidID.mu.Unlock()
	if id == "" {
		return "android-unknown"
	}
	return id
}
