//go:build darwin || linux

package diskfree

import "testing"

func TestFreeKnowsTheRoomOfAnExistingDirectoryAndNotOfAMissingOne(t *testing.T) {
	if n, ok := Free(t.TempDir()); !ok || n == 0 {
		t.Fatalf("Free = %d, %v", n, ok)
	}
	if _, ok := Free(t.TempDir() + "/nope"); ok {
		t.Fatal("a missing directory has free space")
	}
}
