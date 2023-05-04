package wasiunix_test

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/stealthrocket/wasi"
	"github.com/stealthrocket/wasi/testwasi"
	"github.com/stealthrocket/wasi/wasiunix"
	"golang.org/x/sys/unix"
)

func TestWASIP1(t *testing.T) {
	files, _ := filepath.Glob("../testdata/*/*.wasm")

	testwasi.TestWASIP1(t, files,
		func(config testwasi.TestConfig) (wasi.Provider, func(), error) {
			epoch := config.Now()

			provider := &wasiunix.Provider{
				Args:    config.Args,
				Environ: config.Environ,
				Monotonic: func(context.Context) (uint64, error) {
					return uint64(config.Now().Sub(epoch)), nil
				},
				MonotonicPrecision: time.Nanosecond,
				Realtime: func(context.Context) (uint64, error) {
					return uint64(config.Now().UnixNano()), nil
				},
				RealtimePrecision: time.Microsecond,
				Rand:              config.Rand,
			}

			stdin, err := dup(int(config.Stdin.Fd()))
			if err != nil {
				return nil, nil, err
			}

			stdout, err := dup(int(config.Stdout.Fd()))
			if err != nil {
				return nil, nil, err
			}

			stderr, err := dup(int(config.Stderr.Fd()))
			if err != nil {
				return nil, nil, err
			}

			root, err := dup(int(config.RootFS.Fd()))
			if err != nil {
				return nil, nil, err
			}

			provider.Preopen(stdin, "/dev/stdin", wasi.FDStat{
				FileType:   wasi.CharacterDeviceType,
				RightsBase: wasi.AllRights,
			})

			provider.Preopen(stdout, "/dev/stdout", wasi.FDStat{
				FileType:   wasi.CharacterDeviceType,
				RightsBase: wasi.AllRights,
			})

			provider.Preopen(stderr, "/dev/stderr", wasi.FDStat{
				FileType:   wasi.CharacterDeviceType,
				RightsBase: wasi.AllRights,
			})

			provider.Preopen(root, "/", wasi.FDStat{
				FileType:         wasi.DirectoryType,
				RightsBase:       wasi.AllRights,
				RightsInheriting: wasi.AllRights,
			})

			return provider, func() { provider.Close(context.Background()) }, nil
		},
	)
}

func dup(fd int) (int, error) {
	newfd, err := unix.Dup(fd)
	if err != nil {
		return -1, err
	}
	unix.CloseOnExec(newfd)
	return newfd, nil
}

func TestProviderPollAndShutdown(t *testing.T) {
	testProvider(func(ctx context.Context, p *wasiunix.Provider) {
		go func() {
			time.Sleep(100 * time.Millisecond)
			if err := p.Shutdown(ctx); err != nil {
				t.Fatal(err)
			}
		}()

		// This call should block forever, unless async shutdown works, which is
		// what we are testing here.
		subscriptions := []wasi.Subscription{
			subscribeFDRead(0),
			subscribeFDRead(1),
		}
		events := make([]wasi.Event, len(subscriptions))

		_, errno := p.PollOneOff(ctx, subscriptions, events)
		if errno != wasi.ESUCCESS {
			t.Fatal(errno)
		}

		if !reflect.DeepEqual(subscriptions, []wasi.Subscription{
			subscribeFDRead(0),
			subscribeFDRead(1),
		}) {
			t.Error("poll_oneoff: altered subscriptions")
		}

		if !reflect.DeepEqual(events, []wasi.Event{
			{UserData: 0, EventType: wasi.FDReadEvent, Errno: wasi.ECANCELED},
			{UserData: 1, EventType: wasi.FDReadEvent, Errno: wasi.ECANCELED},
		}) {
			t.Errorf("poll_oneoff: wrong events: %+v", events)
		}
	})
}

func TestProviderPollBadFileDescriptor(t *testing.T) {
	testProvider(func(ctx context.Context, p *wasiunix.Provider) {
		subscriptions := []wasi.Subscription{
			subscribeFDRead(0),
			// Subscribe to a file descriptor which is not registered in the
			// provider. This must not fail the poll_oneoff call and instead
			// report an error on the
			subscribeFDRead(42),
		}
		events := make([]wasi.Event, len(subscriptions))

		n, err := p.PollOneOff(ctx, subscriptions, events)
		if err != wasi.ESUCCESS {
			t.Fatal(err)
		}

		if !reflect.DeepEqual(subscriptions, []wasi.Subscription{
			subscribeFDRead(0),
			subscribeFDRead(42),
		}) {
			t.Error("poll_oneoff: altered subscriptions")
		}

		if n != 1 {
			t.Errorf("poll_oneoff: wrong number of events: %d", n)
		} else if !reflect.DeepEqual(events[0], wasi.Event{
			UserData:  42,
			EventType: wasi.FDReadEvent,
			Errno:     wasi.EBADF,
		}) {
			t.Errorf("poll_oneoff: wrong event (0): %+v", events[0])
		}
	})
}

func TestProviderPollMissingMonotonicClock(t *testing.T) {
	testProvider(func(ctx context.Context, p *wasiunix.Provider) {
		p.Monotonic = nil

		subscriptions := []wasi.Subscription{
			subscribeFDRead(0),
			subscribeTimeout(1 * time.Second),
		}
		events := make([]wasi.Event, len(subscriptions))

		n, err := p.PollOneOff(ctx, subscriptions, events)
		if err != wasi.ESUCCESS {
			t.Fatal(err)
		}

		if !reflect.DeepEqual(subscriptions, []wasi.Subscription{
			subscribeFDRead(0),
			subscribeTimeout(1 * time.Second),
		}) {
			t.Error("poll_oneoff: altered subscriptions")
		}

		if n != 1 {
			t.Errorf("poll_oneoff: wrong number of events: %d", n)
		} else if !reflect.DeepEqual(events[0], wasi.Event{
			UserData:  42,
			EventType: wasi.ClockEvent,
			Errno:     wasi.ENOSYS,
		}) {
			t.Errorf("poll_oneoff: wrong event (0): %+v", events[0])
		}
	})
}

func testProvider(f func(context.Context, *wasiunix.Provider)) {
	ctx := context.Background()

	p := newProvider()
	defer p.Close(ctx)

	r, w, err := os.Pipe()
	if err != nil {
		panic(err)
	}
	p.Preopen(int(r.Fd()), "fd0", wasi.FDStat{RightsBase: wasi.AllRights})
	p.Preopen(int(w.Fd()), "fd1", wasi.FDStat{RightsBase: wasi.AllRights})

	f(ctx, p)
}

func newProvider() *wasiunix.Provider {
	return &wasiunix.Provider{
		Realtime:           realtime,
		RealtimePrecision:  time.Microsecond,
		Monotonic:          monotonic,
		MonotonicPrecision: time.Nanosecond,
	}
}

var epoch = time.Now()

func realtime(context.Context) (uint64, error) {
	return uint64(time.Now().UnixNano()), nil
}

func monotonic(context.Context) (uint64, error) {
	return uint64(time.Since(epoch)), nil
}

func subscribeFDRead(fd wasi.FD) wasi.Subscription {
	return wasi.MakeSubscriptionFDReadWrite(
		wasi.UserData(fd),
		wasi.FDReadEvent,
		wasi.SubscriptionFDReadWrite{FD: fd},
	)
}

func subscribeFDWrite(fd wasi.FD) wasi.Subscription {
	return wasi.MakeSubscriptionFDReadWrite(
		wasi.UserData(fd),
		wasi.FDWriteEvent,
		wasi.SubscriptionFDReadWrite{FD: fd},
	)
}

func subscribeTimeout(timeout time.Duration) wasi.Subscription {
	return wasi.MakeSubscriptionClock(
		wasi.UserData(42),
		wasi.SubscriptionClock{
			ID:        wasi.Monotonic,
			Timeout:   wasi.Timestamp(timeout),
			Precision: wasi.Timestamp(time.Nanosecond),
		},
	)
}
