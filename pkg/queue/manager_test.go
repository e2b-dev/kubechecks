package queue

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	vcsmocks "github.com/zapier/kubechecks/mocks/vcs/mocks"
	"github.com/zapier/kubechecks/pkg"
	"github.com/zapier/kubechecks/pkg/checks"
	"github.com/zapier/kubechecks/pkg/container"
	"github.com/zapier/kubechecks/pkg/msg"
	"github.com/zapier/kubechecks/pkg/vcs"
)

func testPullRequest(checkID int, sha string) vcs.PullRequest {
	return vcs.PullRequest{
		CloneURL: "https://gitlab.com/example/repo.git",
		FullName: "example/repo",
		CheckID:  checkID,
		SHA:      sha,
	}
}

func checkIDIs(id int) interface{} {
	return mock.MatchedBy(func(pr vcs.PullRequest) bool { return pr.CheckID == id })
}

// queuedRequest returns a request whose running status has already been posted.
func queuedRequest(ctr container.Container, pr vcs.PullRequest) *CheckRequest {
	request := newCheckRequest(EnqueueParams{PullRequest: pr, Container: ctr})
	request.cancelQueuedStatus()
	close(request.queuedStatusPosted)
	return request
}

func TestEnqueue_SetsQueuedThenRunningBeforeProcessing(t *testing.T) {
	vcsClient := vcsmocks.NewMockClient(t)
	ctr := container.Container{VcsClient: vcsClient}
	pr := testPullRequest(1, "aaa")

	var mu sync.Mutex
	var order []string
	record := func(step string) {
		mu.Lock()
		defer mu.Unlock()
		order = append(order, step)
	}

	releaseQueued := make(chan struct{})
	vcsClient.EXPECT().
		CommitStatus(mock.Anything, pr, pkg.StateQueued).
		RunAndReturn(func(context.Context, vcs.PullRequest, pkg.CommitState) error {
			<-releaseQueued
			record("queued")
			return nil
		}).
		Once()
	vcsClient.EXPECT().
		CommitStatus(mock.Anything, pr, pkg.StateRunning).
		RunAndReturn(func(context.Context, vcs.PullRequest, pkg.CommitState) error {
			record("running")
			return nil
		}).
		Once()

	processed := make(chan struct{})
	qm := NewQueueManager(Config{QueueSize: 10}, func(context.Context, vcs.PullRequest, container.Container, []checks.ProcessorEntry, AIReviewChecker) error {
		record("processed")
		close(processed)
		return nil
	})

	require.NoError(t, qm.Enqueue(context.Background(), EnqueueParams{PullRequest: pr, Container: ctr}))

	select {
	case <-processed:
		t.Fatal("request was processed before the queued status was set")
	case <-time.After(100 * time.Millisecond):
	}

	close(releaseQueued)

	select {
	case <-processed:
	case <-time.After(time.Second):
		t.Fatal("request was not processed after the queued status was set")
	}

	require.NoError(t, qm.Shutdown(context.Background()))
	assert.Equal(t, []string{"queued", "running", "processed"}, order)
}

func TestEnqueue_QueueFullDoesNotSetStatus(t *testing.T) {
	vcsClient := vcsmocks.NewMockClient(t)
	ctr := container.Container{VcsClient: vcsClient}

	// Only the two accepted requests get a queued status, and then a running one.
	vcsClient.EXPECT().
		CommitStatus(mock.Anything, mock.Anything, pkg.StateQueued).
		Return(nil).
		Times(2)
	vcsClient.EXPECT().
		CommitStatus(mock.Anything, mock.Anything, pkg.StateRunning).
		Return(nil).
		Times(2)

	started := make(chan struct{}, 2)
	unblock := make(chan struct{})
	qm := NewQueueManager(Config{QueueSize: 1}, func(context.Context, vcs.PullRequest, container.Container, []checks.ProcessorEntry, AIReviewChecker) error {
		started <- struct{}{}
		<-unblock
		return nil
	})

	// The first request is picked up by the worker and blocks, the second fills the buffer.
	require.NoError(t, qm.Enqueue(context.Background(), EnqueueParams{PullRequest: testPullRequest(1, "aaa"), Container: ctr}))
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first request was not picked up")
	}
	require.NoError(t, qm.Enqueue(context.Background(), EnqueueParams{PullRequest: testPullRequest(2, "bbb"), Container: ctr}))

	assert.Error(t, qm.Enqueue(context.Background(), EnqueueParams{PullRequest: testPullRequest(3, "ccc"), Container: ctr}))

	close(unblock)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("second request was not processed")
	}

	require.NoError(t, qm.Shutdown(context.Background()))
}

func TestNotifyDroppedRequests_FinalizesEveryCommitAndCommentsOncePerPR(t *testing.T) {
	vcsClient := vcsmocks.NewMockClient(t)
	ctr := container.Container{VcsClient: vcsClient}
	first, second, other := testPullRequest(1, "aaa"), testPullRequest(1, "bbb"), testPullRequest(2, "ccc")

	for _, pr := range []vcs.PullRequest{first, second, other} {
		vcsClient.EXPECT().CommitStatus(mock.Anything, pr, pkg.StateError).Return(nil).Once()
	}
	vcsClient.EXPECT().PostMessage(mock.Anything, checkIDIs(1), mock.Anything).Return(nil, nil).Once()
	vcsClient.EXPECT().PostMessage(mock.Anything, checkIDIs(2), mock.Anything).Return(nil, nil).Once()

	rq := &RepoQueue{repoURL: first.CloneURL, queue: make(chan *CheckRequest, 3)}
	for _, pr := range []vcs.PullRequest{first, second, other} {
		rq.queue <- queuedRequest(ctr, pr)
	}

	rq.notifyDroppedRequests(context.Background())
}

func TestNotifyDroppedRequests_CancelsQueuedStatusBeforeError(t *testing.T) {
	vcsClient := vcsmocks.NewMockClient(t)
	ctr := container.Container{VcsClient: vcsClient}
	pr := testPullRequest(1, "aaa")

	var mu sync.Mutex
	var order []pkg.CommitState
	record := func(state pkg.CommitState) {
		mu.Lock()
		defer mu.Unlock()
		order = append(order, state)
	}

	inFlight := make(chan struct{})
	vcsClient.EXPECT().
		CommitStatus(mock.Anything, pr, pkg.StateQueued).
		RunAndReturn(func(ctx context.Context, _ vcs.PullRequest, state pkg.CommitState) error {
			close(inFlight)
			<-ctx.Done()
			record(state)
			return ctx.Err()
		}).
		Once()
	vcsClient.EXPECT().
		CommitStatus(mock.Anything, pr, pkg.StateError).
		RunAndReturn(func(_ context.Context, _ vcs.PullRequest, state pkg.CommitState) error {
			record(state)
			return nil
		}).
		Once()
	vcsClient.EXPECT().PostMessage(mock.Anything, pr, mock.Anything).Return(nil, nil).Once()

	request := newCheckRequest(EnqueueParams{PullRequest: pr, Container: ctr})
	go postQueuedStatus(request)
	<-inFlight

	rq := &RepoQueue{repoURL: pr.CloneURL, queue: make(chan *CheckRequest, 1)}
	rq.queue <- request

	start := time.Now()
	rq.notifyDroppedRequests(context.Background())

	assert.Less(t, time.Since(start), queuedStatusTimeout/2, "shutdown should cancel the queued status, not wait it out")
	assert.Equal(t, []pkg.CommitState{pkg.StateQueued, pkg.StateError}, order)
}

func TestNotifyDroppedRequests_CommentsWhenStatusFails(t *testing.T) {
	vcsClient := vcsmocks.NewMockClient(t)
	ctr := container.Container{VcsClient: vcsClient}
	pr := testPullRequest(1, "aaa")

	vcsClient.EXPECT().CommitStatus(mock.Anything, pr, pkg.StateError).Return(errors.New("unavailable")).Once()
	vcsClient.EXPECT().PostMessage(mock.Anything, pr, mock.Anything).Return(nil, nil).Once()

	rq := &RepoQueue{repoURL: pr.CloneURL, queue: make(chan *CheckRequest, 1)}
	rq.queue <- queuedRequest(ctr, pr)

	rq.notifyDroppedRequests(context.Background())
}

func TestNotifyDroppedRequests_SetsAllStatusesInParallelBeforeComments(t *testing.T) {
	vcsClient := vcsmocks.NewMockClient(t)
	ctr := container.Container{VcsClient: vcsClient}
	const n = droppedRequestConcurrency
	const callTime = 200 * time.Millisecond

	var mu sync.Mutex
	var order []string
	vcsClient.EXPECT().
		CommitStatus(mock.Anything, mock.Anything, pkg.StateError).
		RunAndReturn(func(context.Context, vcs.PullRequest, pkg.CommitState) error {
			time.Sleep(callTime)
			mu.Lock()
			defer mu.Unlock()
			order = append(order, "status")
			return nil
		}).
		Times(n)
	vcsClient.EXPECT().
		PostMessage(mock.Anything, mock.Anything, mock.Anything).
		RunAndReturn(func(context.Context, vcs.PullRequest, string) (*msg.Message, error) {
			mu.Lock()
			defer mu.Unlock()
			order = append(order, "comment")
			return nil, nil
		}).
		Times(n)

	rq := &RepoQueue{repoURL: "https://gitlab.com/example/repo.git", queue: make(chan *CheckRequest, n)}
	for i := 0; i < n; i++ {
		rq.queue <- queuedRequest(ctr, testPullRequest(i, fmt.Sprintf("sha%d", i)))
	}

	start := time.Now()
	rq.notifyDroppedRequests(context.Background())

	assert.Less(t, time.Since(start), n*callTime/2, "status updates should run in parallel")
	for i, step := range order {
		if i < n {
			assert.Equal(t, "status", step)
		} else {
			assert.Equal(t, "comment", step)
		}
	}
}

func TestNotifyDroppedRequests_StopsAtShutdownDeadline(t *testing.T) {
	vcsClient := vcsmocks.NewMockClient(t)
	ctr := container.Container{VcsClient: vcsClient}
	const n = 3 * droppedRequestConcurrency

	var statusCalls atomic.Int32
	vcsClient.EXPECT().
		CommitStatus(mock.Anything, mock.Anything, pkg.StateError).
		RunAndReturn(func(ctx context.Context, _ vcs.PullRequest, _ pkg.CommitState) error {
			statusCalls.Add(1)
			<-ctx.Done()
			return ctx.Err()
		}).
		Maybe()

	rq := &RepoQueue{repoURL: "https://gitlab.com/example/repo.git", queue: make(chan *CheckRequest, n)}
	for i := 0; i < n; i++ {
		rq.queue <- queuedRequest(ctr, testPullRequest(i, fmt.Sprintf("sha%d", i)))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	rq.notifyDroppedRequests(ctx)

	assert.Less(t, time.Since(start), droppedRequestTimeout, "cleanup must not outlive the shutdown context")
	assert.LessOrEqual(t, int(statusCalls.Load()), droppedRequestConcurrency, "no status call may start after the shutdown context is done")
	vcsClient.AssertNotCalled(t, "PostMessage", mock.Anything, mock.Anything, mock.Anything)
}

// A check that outlives its timeout is abandoned at the timeout. One that
// finishes is not, however long ago it ran: its timer must not fire later.
func TestProcessRequest_AbandonsOnlyACheckThatOutlivesItsTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		vcsClient := vcsmocks.NewMockClient(t)
		vcsClient.EXPECT().CommitStatus(mock.Anything, mock.Anything, pkg.StateRunning).Return(nil).Times(2)
		ctr := container.Container{VcsClient: vcsClient}

		abandoned := make(chan int, 1)
		wasAbandoned := func() (int, bool) {
			synctest.Wait()
			select {
			case id := <-abandoned:
				return id, true
			default:
				return 0, false
			}
		}
		stuck := make(chan struct{})
		rq := &RepoQueue{
			processFunc: func(_ context.Context, pr vcs.PullRequest, _ container.Container, _ []checks.ProcessorEntry, _ AIReviewChecker) error {
				if pr.CheckID == 2 {
					<-stuck
				}
				return nil
			},
			abandon: func(request *CheckRequest) { abandoned <- request.PullRequest.CheckID },
		}

		rq.processRequest(queuedRequest(ctr, testPullRequest(1, "aaa")))
		time.Sleep(2 * checkTimeout)
		_, yes := wasAbandoned()
		require.False(t, yes, "a check that finished was abandoned afterwards")

		done := make(chan struct{})
		go func() {
			defer close(done)
			rq.processRequest(queuedRequest(ctr, testPullRequest(2, "bbb")))
		}()
		time.Sleep(checkTimeout - time.Second)
		_, yes = wasAbandoned()
		assert.False(t, yes, "a check was abandoned before its timeout")
		time.Sleep(2 * time.Second)
		id, yes := wasAbandoned()
		assert.True(t, yes, "a check that outlived its timeout was not abandoned")
		assert.Equal(t, 2, id)

		close(stuck)
		<-done
	})
}

// abandonCheck has to end the process, which a test cannot survive: it runs
// in a copy of the test binary, and the test reads what the copy left behind.
func TestAbandonCheck_WritesEveryStackAndExits(t *testing.T) {
	const inChild = "KUBECHECKS_TEST_ABANDON_CHECK"
	if os.Getenv(inChild) != "" {
		parked := make(chan struct{})
		go parkedForTheStackDump(parked)
		<-parked
		abandonCheck(queuedRequest(container.Container{}, testPullRequest(7, "ccc")))
		return
	}

	child := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestAbandonCheck_WritesEveryStackAndExits$")
	child.Env = append(os.Environ(), inChild+"=1")
	output, err := child.CombinedOutput()

	var exit *exec.ExitError
	require.ErrorAs(t, err, &exit, "the process did not exit with an error: %s", output)
	assert.Equal(t, 1, exit.ExitCode())
	assert.Contains(t, string(output), "parkedForTheStackDump", "the dump lacks the stack of another goroutine")
	assert.Contains(t, string(output), "check did not finish in time")
}

// parkedForTheStackDump waits for ever, so that its frame is in a dump of
// every goroutine and in no dump of the calling one alone.
func parkedForTheStackDump(parked chan<- struct{}) {
	close(parked)
	select {}
}
