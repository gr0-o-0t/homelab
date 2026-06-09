package spinner

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func Test_Frames_NotEmpty(t *testing.T) {
	assert.Greater(t, len(frames), 0)
}

func Test_FrameStyle_Defined(t *testing.T) {
	assert.NotNil(t, frameStyle)
}
