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

func TestSpaceKnowsTheSizeOfTheDiskAboveItsFreeRoom(t *testing.T) {
	free, total, ok := Space(t.TempDir())
	if !ok || total == 0 || free > total {
		t.Fatalf("Space = %d of %d, %v", free, total, ok)
	}
}

func TestTwoFoldersOfOneDirectoryAreOnTheSameDisk(t *testing.T) {
	dir := t.TempDir()
	if !SameDisk(dir, dir+"/..") {
		t.Fatal("a folder and its parent on different disks")
	}
	if !SameDisk(dir, dir+"/nope") {
		t.Fatal("what cannot be told is not the cautious answer")
	}
}
