package taskrunner

import (
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/hashicorp/consul-template/signals"
	log "github.com/hashicorp/go-hclog"

	"github.com/hashicorp/nomad/client/allocdir"
	"github.com/hashicorp/nomad/client/allocrunnerv2/interfaces"
	"github.com/hashicorp/nomad/client/vaultclient"
	"github.com/hashicorp/nomad/nomad/structs"
)

const (
	// vaultBackoffBaseline is the baseline time for exponential backoff when
	// attempting to retrieve a Vault token
	vaultBackoffBaseline = 5 * time.Second

	// vaultBackoffLimit is the limit of the exponential backoff when attempting
	// to retrieve a Vault token
	vaultBackoffLimit = 3 * time.Minute

	// vaultTokenFile is the name of the file holding the Vault token inside the
	// task's secret directory
	vaultTokenFile = "vault_token"
)

type vaultTokenUpdateHandler interface {
	updatedVaultToken(token string)
}

func (tr *TaskRunner) updatedVaultToken(token string) {
	// Update the Vault token on the runner
	tr.setVaultToken(token)

	// Update the tasks environment
	tr.envBuilder.SetVaultToken(token, tr.task.Vault.Env)

	// Update the hooks with the new Vault token
	tr.updateHooks()
}

type vaultHookConfig struct {
	vaultStanza *structs.Vault
	client      vaultclient.VaultClient
	events      EventEmitter
	lifecycle   TaskLifecycle
	updater     vaultTokenUpdateHandler
	logger      log.Logger
	alloc       *structs.Allocation
	task        string
}

type vaultHook struct {
	// vaultStanza is the vault stanza for the task
	vaultStanza *structs.Vault

	// eventEmitter is used to emit events to the task
	eventEmitter EventEmitter

	// lifecycle is used to signal, restart and kill a task
	lifecycle TaskLifecycle

	// updater is used to update the Vault token
	updater vaultTokenUpdateHandler

	// client is the Vault client to retrieve and renew the Vault token
	client vaultclient.VaultClient

	// logger is used to log
	logger log.Logger

	// ctx and cancel are used to kill the long running token manager
	ctx    context.Context
	cancel context.CancelFunc

	// tokenPath is the path in which to read and write the token
	tokenPath string

	// alloc is the allocation
	alloc *structs.Allocation

	// taskName is the name of the task
	taskName string

	// firstRun stores whether it is the first run for the hook
	firstRun bool

	// future is used to wait on retrieving a Vault token
	future *tokenFuture
}

func newVaultHook(config *vaultHookConfig) *vaultHook {
	ctx, cancel := context.WithCancel(context.Background())
	h := &vaultHook{
		vaultStanza:  config.vaultStanza,
		client:       config.client,
		eventEmitter: config.events,
		lifecycle:    config.lifecycle,
		updater:      config.updater,
		alloc:        config.alloc,
		taskName:     config.task,
		firstRun:     true,
		ctx:          ctx,
		cancel:       cancel,
		future:       newTokenFuture(),
	}
	h.logger = config.logger.Named(h.Name())
	return h
}

func (*vaultHook) Name() string {
	return "vault"
}

func (h *vaultHook) Prerun(ctx context.Context, req *interfaces.TaskPrerunRequest, resp *interfaces.TaskPrerunResponse) error {
	// If we have already run prerun before exit early. We do not use the
	// PrerunDone value because we want to recover the token on restoration.
	first := h.firstRun
	h.firstRun = false
	if !first {
		return nil
	}

	// Try to recover a token if it was previously written in the secrets
	// directory
	recoveredToken := ""
	h.tokenPath = filepath.Join(req.TaskDir, allocdir.TaskSecrets, vaultTokenFile)
	data, err := ioutil.ReadFile(h.tokenPath)
	if err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("failed to recover vault token: %v", err)
		}

		// Token file doesn't exist
	} else {
		// Store the recovered token
		recoveredToken = string(data)
	}

	// Launch the token manager
	go h.run(recoveredToken)

	// Block until we get a token
	select {
	case <-h.future.Wait():
	case <-ctx.Done():
		return nil
	}

	h.updater.updatedVaultToken(h.future.Get())
	return nil
}

func (h *vaultHook) Poststop(ctx context.Context, req *interfaces.TaskPoststopRequest, resp *interfaces.TaskPoststopResponse) error {
	// Shutdown any created manager
	h.cancel()
	return nil
}

// run should be called in a go-routine and manages the derivation, renewal and
// handling of errors with the Vault token. The optional parameter allows
// setting the initial Vault token. This is useful when the Vault token is
// recovered off disk.
func (h *vaultHook) run(token string) {
	// Helper for stopping token renewal
	stopRenewal := func() {
		if err := h.client.StopRenewToken(h.future.Get()); err != nil {
			h.logger.Warn("failed to stop token renewal", "error", err)
		}
	}

	// updatedToken lets us store state between loops. If true, a new token
	// has been retrieved and we need to apply the Vault change mode
	var updatedToken bool

OUTER:
	for {
		// Check if we should exit
		select {
		case <-h.ctx.Done():
			stopRenewal()
			return
		default:
		}

		// Clear the token
		h.future.Clear()

		// Check if there already is a token which can be the case for
		// restoring the TaskRunner
		if token == "" {
			// Get a token
			var exit bool
			token, exit = h.deriveVaultToken()
			if exit {
				// Exit the manager
				return
			}

			// Write the token to disk
			if err := h.writeToken(token); err != nil {
				errorString := "failed to write Vault token to disk"
				h.logger.Error(errorString, "error", err)
				h.lifecycle.Kill("vault", errorString, true)
				return
			}
		}

		// Start the renewal process
		renewCh, err := h.client.RenewToken(token, 30)

		// An error returned means the token is not being renewed
		if err != nil {
			h.logger.Error("failed to start renewal of Vault token", "error", err)
			token = ""
			goto OUTER
		}

		// The Vault token is valid now, so set it
		h.future.Set(token)

		if updatedToken {
			switch h.vaultStanza.ChangeMode {
			case structs.VaultChangeModeSignal:
				s, err := signals.Parse(h.vaultStanza.ChangeSignal)
				if err != nil {
					h.logger.Error("failed to parse signal", "error", err)
					h.lifecycle.Kill("vault", fmt.Sprintf("failed to parse signal: %v", err), true)
					return
				}

				if err := h.lifecycle.Signal("vault", "new Vault token acquired", s); err != nil {
					h.logger.Error("failed to send signal", "error", err)
					h.lifecycle.Kill("vault", fmt.Sprintf("failed to send signal: %v", err), true)
					return
				}
			case structs.VaultChangeModeRestart:
				const noFailure = false
				h.lifecycle.Restart("vault", "new Vault token acquired", noFailure)
			case structs.VaultChangeModeNoop:
				fallthrough
			default:
				h.logger.Error("invalid Vault change mode", "mode", h.vaultStanza.ChangeMode)
			}

			// We have handled it
			updatedToken = false

			// Call the handler
			h.updater.updatedVaultToken(token)
		}

		// Start watching for renewal errors
		select {
		case err := <-renewCh:
			// Clear the token
			token = ""
			h.logger.Error("failed to renew Vault token", "error", err)
			stopRenewal()

			// Check if we have to do anything
			if h.vaultStanza.ChangeMode != structs.VaultChangeModeNoop {
				updatedToken = true
			}
		case <-h.ctx.Done():
			stopRenewal()
			return
		}
	}
}

// deriveVaultToken derives the Vault token using exponential backoffs. It
// returns the Vault token and whether the manager should exit.
func (h *vaultHook) deriveVaultToken() (token string, exit bool) {
	attempts := 0
	for {
		tokens, err := h.client.DeriveToken(h.alloc, []string{h.taskName})
		if err == nil {
			return tokens[h.taskName], false
		}

		// Check if this is a server side error
		if structs.IsServerSide(err) {
			h.logger.Error("failed to derive Vault token", "error", err, "server_side", true)
			h.lifecycle.Kill("vault", fmt.Sprintf("server error deriving vault token: %v", err), true)
			return "", true
		}

		// Check if we can't recover from the error
		if !structs.IsRecoverable(err) {
			h.logger.Error("failed to derive Vault token", "error", err, "recoverable", false)
			h.lifecycle.Kill("vault", fmt.Sprintf("failed to derive token: %v", err), true)
			return "", true
		}

		// Handle the retry case
		backoff := (1 << (2 * uint64(attempts))) * vaultBackoffBaseline
		if backoff > vaultBackoffLimit {
			backoff = vaultBackoffLimit
		}
		h.logger.Error("failed to derive Vault token", "error", err, "recoverable", true, "backoff", backoff)

		attempts++

		// Wait till retrying
		select {
		case <-h.ctx.Done():
			return "", true
		case <-time.After(backoff):
		}
	}
}

// writeToken writes the given token to disk
func (h *vaultHook) writeToken(token string) error {
	if err := ioutil.WriteFile(h.tokenPath, []byte(token), 0777); err != nil {
		return fmt.Errorf("failed to write vault token: %v", err)
	}

	return nil
}

// tokenFuture stores the Vault token and allows consumers to block till a valid
// token exists
type tokenFuture struct {
	waiting []chan struct{}
	token   string
	set     bool
	m       sync.Mutex
}

// newTokenFuture returns a new token future without any token set
func newTokenFuture() *tokenFuture {
	return &tokenFuture{}
}

// Wait returns a channel that can be waited on. When this channel unblocks, a
// valid token will be available via the Get method
func (f *tokenFuture) Wait() <-chan struct{} {
	f.m.Lock()
	defer f.m.Unlock()

	c := make(chan struct{})
	if f.set {
		close(c)
		return c
	}

	f.waiting = append(f.waiting, c)
	return c
}

// Set sets the token value and unblocks any caller of Wait
func (f *tokenFuture) Set(token string) *tokenFuture {
	f.m.Lock()
	defer f.m.Unlock()

	f.set = true
	f.token = token
	for _, w := range f.waiting {
		close(w)
	}
	f.waiting = nil
	return f
}

// Clear clears the set vault token.
func (f *tokenFuture) Clear() *tokenFuture {
	f.m.Lock()
	defer f.m.Unlock()

	f.token = ""
	f.set = false
	return f
}

// Get returns the set Vault token
func (f *tokenFuture) Get() string {
	f.m.Lock()
	defer f.m.Unlock()
	return f.token
}