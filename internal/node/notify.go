package node

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/shukiv/gniza/internal/nodestore"
	"github.com/shukiv/gniza/internal/notify"
)

// Channels lists where this server sends word of what happened.
func (e *Engine) Channels() ([]nodestore.Channel, error) { return e.store.Channels() }

// SaveChannel stores a channel, sealing whatever credentials it needs.
//
// Secrets given as empty on an edit are left as they were: a form that
// shows a blank password field must not erase the stored one, which is
// how an operator ends up with notifications that quietly stopped.
func (e *Engine) SaveChannel(channel nodestore.Channel, secrets map[string]string) (nodestore.Channel, error) {
	if strings.TrimSpace(channel.Name) == "" {
		return nodestore.Channel{}, fmt.Errorf("node: give the channel a name")
	}
	if _, known := notify.Senders[notify.Kind(channel.Kind)]; !known {
		return nodestore.Channel{}, fmt.Errorf("node: %q is not a way of sending anything", channel.Kind)
	}

	given := map[string]string{}
	for key, value := range secrets {
		if strings.TrimSpace(value) != "" {
			given[key] = value
		}
	}
	switch {
	case len(given) > 0:
		id, err := SealCredentials(e.store, e.vault, given)
		if err != nil {
			return nodestore.Channel{}, err
		}
		channel.SecretsID = id
	case channel.ID != "":
		if previous, err := e.store.Channel(channel.ID); err == nil {
			channel.SecretsID = previous.SecretsID
			channel.CreatedAt = previous.CreatedAt
			channel.LastSent, channel.LastError = previous.LastSent, previous.LastError
		}
	}

	built, err := e.channelFor(channel)
	if err != nil {
		return nodestore.Channel{}, err
	}
	if missing := notify.Missing(built); len(missing) > 0 {
		return nodestore.Channel{}, fmt.Errorf(
			"node: %s needs %s", notify.Kind(channel.Kind).Title(), strings.Join(missing, ", "))
	}
	return e.store.PutChannel(channel)
}

// DeleteChannel removes one.
func (e *Engine) DeleteChannel(id string) error { return e.store.DeleteChannel(id) }

// channelFor turns a stored channel into one that can be sent through,
// unsealing its credentials.
func (e *Engine) channelFor(stored nodestore.Channel) (notify.Channel, error) {
	channel := notify.Channel{
		Kind:    notify.Kind(stored.Kind),
		Name:    stored.Name,
		Config:  stored.Config,
		Secrets: map[string]string{},
	}
	for _, event := range stored.Events {
		channel.Events = append(channel.Events, notify.Event(event))
	}
	if stored.SecretsID == "" {
		return channel, nil
	}
	plaintext, err := e.openSecret(stored.SecretsID)
	if err != nil {
		return notify.Channel{}, err
	}
	if err := decodeSecrets(plaintext, &channel.Secrets); err != nil {
		return notify.Channel{}, err
	}
	return channel, nil
}

// TestChannel sends one message, now, so an operator finds out whether a
// channel works while they are still looking at it rather than during the
// incident it was meant to warn about.
func (e *Engine) TestChannel(ctx context.Context, id string) error {
	stored, err := e.store.Channel(id)
	if err != nil {
		return err
	}
	channel, err := e.channelFor(stored)
	if err != nil {
		return err
	}
	message := notify.Message{
		Event:   notify.EventRestore,
		Subject: "Gniza is set up to reach you here",
		Body: "This is a test, sent because somebody pressed the button. " +
			"Nothing has gone wrong.",
		Host: e.settings.Hostname,
	}
	sendErr := notify.Send(ctx, channel, message)
	e.recordSend(stored, sendErr)
	return sendErr
}

// Notify tells every channel that asked about this kind of event.
//
// It never fails the thing it is reporting on: a backup that worked did
// work, whatever the mail server thinks, and one that failed has already
// failed. Whatever happens here is recorded against the channel instead.
func (e *Engine) Notify(ctx context.Context, message notify.Message) {
	if message.Host == "" {
		message.Host = e.settings.Hostname
	}
	channels, err := e.store.Channels()
	if err != nil {
		e.log.Error("read notification channels", "error", err)
		return
	}
	for _, stored := range channels {
		if !stored.Enabled {
			continue
		}
		channel, err := e.channelFor(stored)
		if err != nil {
			e.recordSend(stored, err)
			continue
		}
		if !channel.Wants(message.Event) {
			continue
		}
		// Bounded on purpose: a channel that hangs must not hold up the
		// scheduler behind it.
		sendCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		sendErr := notify.Send(sendCtx, channel, message)
		cancel()
		if sendErr != nil {
			e.log.Error("send notification", "channel", stored.Name, "error", sendErr)
		}
		e.recordSend(stored, sendErr)
	}
}

// recordSend keeps what happened on the channel, so the page can say a
// channel is configured but not working.
func (e *Engine) recordSend(stored nodestore.Channel, sendErr error) {
	now := time.Now().UTC()
	if sendErr != nil {
		stored.LastError = sendErr.Error()
	} else {
		stored.LastError = ""
		stored.LastSent = &now
	}
	if _, err := e.store.PutChannel(stored); err != nil {
		e.log.Error("record what a channel did", "channel", stored.Name, "error", err)
	}
}
