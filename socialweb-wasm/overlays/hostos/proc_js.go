//go:build js

package hostos

import (
	"context"
	"errors"
	"os"
	"syscall"
	"time"
)

// js/wasm has no process table, signals, or parent-death delivery. Anything
// that would manage a host process fails closed.

func DieWithParent() *syscall.SysProcAttr { return &syscall.SysProcAttr{} }

func NewProcessGroup() *syscall.SysProcAttr { return &syscall.SysProcAttr{} }

func GracefulSignal() os.Signal { return os.Interrupt }

func ForcedSignal() os.Signal { return os.Kill }

func BootID() (string, error) { return "", errors.ErrUnsupported }

func ProcStart(int) (uint64, error) { return 0, errors.ErrUnsupported }

func AwaitExit(context.Context, int, uint64) error { return errors.ErrUnsupported }

func Alive(int) (bool, error) { return false, errors.ErrUnsupported }

func Cmdline(int) ([]string, error) { return nil, errors.ErrUnsupported }

func Signal(int, bool) error { return errors.ErrUnsupported }

func Terminate(context.Context, int, time.Duration) error { return errors.ErrUnsupported }

func Reload(int) error { return errors.ErrUnsupported }

func LocalPortRange() (int, int, error) { return 0, 0, errors.ErrUnsupported }
