package host

import (
	"errors"
	"strings"
	"testing"
)

// 这条分类在集成测试里只在「机器恰好忙」的那一瞬间才走到——
// 而一个只在偶发时走到的分支，正是最需要被固定下来的那种。
// 决策 306 已经为同一类问题付过一次代价（那个数根本不属于我们）。
//
// 判据：整卷剩余字节是我们写、但不是我们的量。所以「写成功了而剩余没掉」
// 必须报忙（别人的写抵消了我们的），「压根没写进去」才报实现坏了。

func TestFillDiskObservable(t *testing.T) {
	const chunk = int64(chunkBytes)
	const mb = int64(1) << 20

	cases := []struct {
		name     string
		before   int64
		after    int64
		written  int64
		wantErr  string // "" means no error
		wantBusy bool
	}{
		{
			name: "a real fill is observable", before: 100 * mb, after: 96 * mb, written: 4 * mb,
		},
		{
			// 我们写了、剩余空间还涨了：只可能是别人在同一窗口里写得更多。
			name: "our writes landed but a concurrent writer won", before: 100 * mb, after: 104 * mb, written: 4 * mb,
			wantErr: "cannot be attributed to this injection", wantBusy: true,
		},
		{
			name: "nothing was written at all", before: 100 * mb, after: 100 * mb, written: 0,
			wantErr: "the fault is not observable",
		},
		{
			// 写进去了但一整块都没掉：还是别人的问题，写成功这件事本身排除了实现坏了。
			name: "a masked drop is busy, not broken", before: 100 * mb, after: 100*mb - chunk/2, written: 4 * mb,
			wantErr: "masking the change", wantBusy: true,
		},
		{
			// 写了一半块、剩余空间一块没掉：这仍然是二义的——块对齐可以让半块
			// 真的占 0 字节，别人的写也可以抵消它。诚实的答案是"忙"，
			// 而不是拿一个我们分不出来的情形去报红。
			name:   "a sub-chunk write with no drop is ambiguous, so it is busy",
			before: 100 * mb, after: 100 * mb, written: chunk / 2,
			wantErr: "cannot be attributed to this injection", wantBusy: true,
		},
		{
			// 掉了一点、而我们写的还不到一块：这里没有并发能解释它
			//（写都没写够，抵消不了），这是真的没生效，必须报红。
			name:   "a sub-chunk write with a sliver of drop is broken",
			before: 100 * mb, after: 100*mb - 1, written: chunk / 2,
			wantErr: "a fault nobody can see in `df` is not a fault",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := fillDiskObservable("/tmp/vol", tc.before, tc.after, tc.written)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("want no error, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("want an error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %q, want it to contain %q", err, tc.wantErr)
			}
			if got := errors.Is(err, ErrMachineBusy); got != tc.wantBusy {
				t.Fatalf("ErrMachineBusy = %v, want %v (err = %v)", got, tc.wantBusy, err)
			}
		})
	}
}

// busy 与 broken 的分流必须让调用方能够分开处理，否则"忙"这个出口没有意义。
func TestFillDiskBusyIsDistinguishableFromBroken(t *testing.T) {
	busy := fillDiskObservable("/tmp/vol", 100<<20, 200<<20, 4<<20)
	broken := fillDiskObservable("/tmp/vol", 100<<20, 100<<20, 0)
	if !errors.Is(busy, ErrMachineBusy) {
		t.Fatalf("busy case is not ErrMachineBusy: %v", busy)
	}
	if errors.Is(broken, ErrMachineBusy) {
		t.Fatalf("broken case must not be skippable: %v", broken)
	}
}
