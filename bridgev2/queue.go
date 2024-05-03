// Copyright (c) 2024 Tulir Asokan
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package bridgev2

import (
	"context"
	"strings"

	"github.com/rs/zerolog"

	"maunium.net/go/mautrix/event"
)

func (br *Bridge) QueueMatrixEvent(ctx context.Context, evt *event.Event) {
	// TODO maybe HandleMatrixEvent would be more appropriate as this also handles bot invites and commands

	log := zerolog.Ctx(ctx)
	var sender *User
	if evt.Sender != "" {
		var err error
		sender, err = br.GetUserByMXID(ctx, evt.Sender)
		if err != nil {
			log.Err(err).Msg("Failed to get sender user for incoming Matrix event")
			// TODO send metrics
			return
		}
	}
	if sender == nil && evt.Type.Class != event.EphemeralEventType {
		log.Error().Msg("Missing sender for incoming non-ephemeral Matrix event")
		// TODO send metrics
		return
	}
	if evt.Type == event.EventMessage {
		msg := evt.Content.AsMessage()
		if msg != nil {
			msg.RemoveReplyFallback()

			if strings.HasPrefix(msg.Body, br.CommandPrefix) || evt.RoomID == sender.ManagementRoom {
				br.Commands.Handle(
					ctx,
					evt.RoomID,
					evt.ID,
					sender,
					strings.TrimPrefix(msg.Body, br.CommandPrefix+" "),
					msg.RelatesTo.GetReplyTo(),
				)
				return
			}
		}
	}
	if evt.Type == event.StateMember && evt.GetStateKey() == br.Bot.GetMXID().String() && evt.Content.AsMember().Membership == event.MembershipInvite {
		br.Bot.EnsureJoined(ctx, evt.RoomID)
		// TODO handle errors
		if sender.ManagementRoom == "" {
			sender.ManagementRoom = evt.RoomID
			br.DB.User.Update(ctx, sender.User)
		}
		return
	}
	portal, err := br.GetPortalByMXID(ctx, evt.RoomID)
	if err != nil {
		log.Err(err).Msg("Failed to get portal for incoming Matrix event")
		// TODO send metrics
		return
	} else if portal != nil {
		portal.queueEvent(ctx, &portalMatrixEvent{
			evt:    evt,
			sender: sender,
		})
	}
}

func (br *Bridge) QueueRemoteEvent(login *UserLogin, evt RemoteEvent) {
	log := login.Log
	ctx := log.WithContext(context.TODO())
	portal, err := br.GetPortalByID(ctx, evt.GetPortalID())
	if err != nil {
		log.Err(err).Str("portal_id", string(portal.ID)).
			Msg("Failed to get portal to handle remote event")
		return
	}
	// TODO put this in a better place, and maybe cache to avoid constant db queries
	br.DB.UserLogin.EnsureUserPortalExists(ctx, login.UserLogin, portal.ID)
	portal.queueEvent(ctx, &portalRemoteEvent{
		evt:    evt,
		source: login,
	})
}
