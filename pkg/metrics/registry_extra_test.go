package metrics

import (
	"errors"
	"testing"
	"time"
)

var errTest = errors.New("test error")

func TestNodeOpObserve_Success(t *testing.T) {
	NodeOpObserve("test_op", time.Now().Add(-time.Second), nil)
}

func TestNodeOpObserve_Error(t *testing.T) {
	NodeOpObserve("test_op_err", time.Now().Add(-time.Second), errTest)
}

func TestControllerOpObserve_Success(t *testing.T) {
	ControllerOpObserve("ctrl_op", time.Now().Add(-time.Second), nil)
}

func TestControllerOpObserve_Error(t *testing.T) {
	ControllerOpObserve("ctrl_op_err", time.Now().Add(-time.Second), errTest)
}

func TestNodePullOpObserve_Success(t *testing.T) {
	NodePullOpObserve("pull_layer", 1024*1024, time.Now().Add(-time.Second), nil)
}

func TestNodePullOpObserve_Error(t *testing.T) {
	NodePullOpObserve("pull_layer_err", 512, time.Now().Add(-time.Second), errTest)
}
