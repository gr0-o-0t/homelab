package session

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const mountinfo = `22 1 8:2 / / rw,relatime shared:1 - ext4 /dev/sda2 rw
40 22 0:35 / /home rw,relatime shared:2 - ext4 /dev/sda3 rw
91 40 0:50 / /home/u rw,nosuid,nodev,relatime shared:40 - ecryptfs /home/.ecryptfs/u/.Private rw,ecryptfs_sig=abc
95 40 0:51 / /home/homed rw,relatime shared:41 - ext4 /dev/mapper/home-homed rw
96 22 0:52 / /mnt/my\040disk rw - ext4 /dev/sdb1 rw
`

func TestLateMount(t *testing.T) {
	fs, late := lateMount(mountinfo, "/home/u/.config/homelab", "/home/u")
	assert.True(t, late)
	assert.Equal(t, "ecryptfs", fs)

	fs, late = lateMount(mountinfo, "/home/homed/.config/homelab", "/home/homed")
	assert.True(t, late, "a mount exactly at home (systemd-homed, pam_mount)")
	assert.Equal(t, "ext4", fs)

	_, late = lateMount(mountinfo, "/home/plain/.config/homelab", "/home/plain")
	assert.False(t, late, "on /home, mounted at boot")

	fs, late = lateMount(mountinfo, "/mnt/my disk/homelab", "/home/plain")
	assert.False(t, late)
	assert.Equal(t, "ext4", fs)

	_, late = lateMount(mountinfo, "/home/uu/x", "/home/uu")
	assert.False(t, late, "/home/u is not a prefix of /home/uu")
}

func TestStatStartTicks(t *testing.T) {
	stat := "1234 (weird) name)) S 1 1234 1234 0 -1 4194560 100 0 0 0 1 2 0 0 20 0 1 0 98765 1000 10"
	n, err := statStartTicks(stat)
	require.NoError(t, err)
	assert.Equal(t, uint64(98765), n)

	_, err = statStartTicks("garbage")
	assert.Error(t, err)
}

func TestBootTime(t *testing.T) {
	bt, err := bootTime("cpu 1 2 3\nbtime 1700000000\nprocesses 5\n")
	require.NoError(t, err)
	assert.Equal(t, time.Unix(1700000000, 0), bt)

	_, err = bootTime("cpu 1\n")
	assert.Error(t, err)
}
