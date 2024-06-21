// Copyright (c) 2024 Tulir Asokan
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package bridgev2

import (
	"context"
	"fmt"
	"time"

	"github.com/rs/zerolog"

	"maunium.net/go/mautrix/bridge/status"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/id"
)

type UserLogin struct {
	*database.UserLogin
	Bridge *Bridge
	User   *User
	Log    zerolog.Logger

	Client      NetworkAPI
	BridgeState *BridgeStateQueue
}

func (br *Bridge) loadUserLogin(ctx context.Context, user *User, dbUserLogin *database.UserLogin) (*UserLogin, error) {
	if dbUserLogin == nil {
		return nil, nil
	}
	if user == nil {
		var err error
		user, err = br.unlockedGetUserByMXID(ctx, dbUserLogin.UserMXID, true)
		if err != nil {
			return nil, fmt.Errorf("failed to get user: %w", err)
		}
	}
	userLogin := &UserLogin{
		UserLogin: dbUserLogin,
		Bridge:    br,
		User:      user,
		Log:       user.Log.With().Str("login_id", string(dbUserLogin.ID)).Logger(),
	}
	err := br.Network.LoadUserLogin(ctx, userLogin)
	if err != nil {
		return nil, fmt.Errorf("failed to prepare: %w", err)
	}
	user.logins[userLogin.ID] = userLogin
	br.userLoginsByID[userLogin.ID] = userLogin
	userLogin.BridgeState = br.NewBridgeStateQueue(userLogin)
	return userLogin, nil
}

func (br *Bridge) loadManyUserLogins(ctx context.Context, user *User, logins []*database.UserLogin) ([]*UserLogin, error) {
	output := make([]*UserLogin, len(logins))
	for i, dbLogin := range logins {
		if cached, ok := br.userLoginsByID[dbLogin.ID]; ok {
			output[i] = cached
		} else {
			loaded, err := br.loadUserLogin(ctx, user, dbLogin)
			if err != nil {
				return nil, fmt.Errorf("failed to load user login: %w", err)
			}
			output[i] = loaded
		}
	}
	return output, nil
}

func (br *Bridge) unlockedLoadUserLoginsByMXID(ctx context.Context, user *User) error {
	logins, err := br.DB.UserLogin.GetAllForUser(ctx, user.MXID)
	if err != nil {
		return err
	}
	_, err = br.loadManyUserLogins(ctx, user, logins)
	return err
}

func (br *Bridge) GetAllUserLogins(ctx context.Context) ([]*UserLogin, error) {
	logins, err := br.DB.UserLogin.GetAll(ctx)
	if err != nil {
		return nil, err
	}
	br.cacheLock.Lock()
	defer br.cacheLock.Unlock()
	return br.loadManyUserLogins(ctx, nil, logins)
}

func (br *Bridge) GetUserLoginsInPortal(ctx context.Context, portal networkid.PortalKey) ([]*UserLogin, error) {
	logins, err := br.DB.UserLogin.GetAllInPortal(ctx, portal)
	if err != nil {
		return nil, err
	}
	br.cacheLock.Lock()
	defer br.cacheLock.Unlock()
	return br.loadManyUserLogins(ctx, nil, logins)
}

func (br *Bridge) GetExistingUserLoginByID(ctx context.Context, id networkid.UserLoginID) (*UserLogin, error) {
	login, err := br.DB.UserLogin.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	br.cacheLock.Lock()
	defer br.cacheLock.Unlock()
	return br.loadUserLogin(ctx, nil, login)
}

func (br *Bridge) GetCachedUserLoginByID(id networkid.UserLoginID) *UserLogin {
	br.cacheLock.Lock()
	defer br.cacheLock.Unlock()
	return br.userLoginsByID[id]
}

func (user *User) NewLogin(ctx context.Context, data *database.UserLogin, client NetworkAPI) (*UserLogin, error) {
	data.BridgeID = user.BridgeID
	data.UserMXID = user.MXID
	ul := &UserLogin{
		UserLogin: data,
		Bridge:    user.Bridge,
		User:      user,
		Log:       user.Log.With().Str("login_id", string(data.ID)).Logger(),
		Client:    client,
	}
	err := user.Bridge.DB.UserLogin.Insert(ctx, ul.UserLogin)
	if err != nil {
		return nil, err
	}
	ul.BridgeState = user.Bridge.NewBridgeStateQueue(ul)
	user.Bridge.cacheLock.Lock()
	defer user.Bridge.cacheLock.Unlock()
	user.Bridge.userLoginsByID[ul.ID] = ul
	user.logins[ul.ID] = ul
	return ul, nil
}

func (ul *UserLogin) Save(ctx context.Context) error {
	return ul.Bridge.DB.UserLogin.Update(ctx, ul.UserLogin)
}

func (ul *UserLogin) Logout(ctx context.Context) {
	ul.Delete(ctx, status.BridgeState{StateEvent: status.StateLoggedOut}, true)
}

func (ul *UserLogin) Delete(ctx context.Context, state status.BridgeState, logoutRemote bool) {
	if logoutRemote {
		ul.Client.LogoutRemote(ctx)
	} else {
		ul.Disconnect(nil)
	}
	portals, err := ul.Bridge.DB.UserPortal.GetAllForLogin(ctx, ul.UserLogin)
	if err != nil {
		ul.Log.Err(err).Msg("Failed to get user portals")
	}
	err = ul.Bridge.DB.UserLogin.Delete(ctx, ul.ID)
	if err != nil {
		ul.Log.Err(err).Msg("Failed to delete user login")
	}
	ul.Bridge.cacheLock.Lock()
	delete(ul.User.logins, ul.ID)
	delete(ul.Bridge.userLoginsByID, ul.ID)
	ul.Bridge.cacheLock.Unlock()
	go ul.deleteSpace(ctx)
	go ul.kickUserFromPortals(ctx, portals)
	if state.StateEvent != "" {
		ul.BridgeState.Send(state)
	}
	ul.BridgeState.Destroy()
}

func (ul *UserLogin) deleteSpace(ctx context.Context) {
	if ul.SpaceRoom == "" {
		return
	}
	err := ul.Bridge.Bot.DeleteRoom(ctx, ul.SpaceRoom, false)
	if err != nil {
		ul.Log.Err(err).Msg("Failed to delete space room")
	}
}

func (ul *UserLogin) kickUserFromPortals(ctx context.Context, portals []*database.UserPortal) {
	// TODO kick user out of rooms
}

func (ul *UserLogin) MarkAsPreferredIn(ctx context.Context, portal *Portal) error {
	return ul.Bridge.DB.UserPortal.MarkAsPreferred(ctx, ul.UserLogin, portal.PortalKey)
}

var _ status.BridgeStateFiller = (*UserLogin)(nil)

func (ul *UserLogin) GetMXID() id.UserID {
	return ul.UserMXID
}

func (ul *UserLogin) GetRemoteID() string {
	return string(ul.ID)
}

func (ul *UserLogin) GetRemoteName() string {
	return ul.Metadata.RemoteName
}

func (ul *UserLogin) Disconnect(done func()) {
	if done != nil {
		defer done()
	}
	client := ul.Client
	if client != nil {
		ul.Client = nil
		disconnected := make(chan struct{})
		go func() {
			client.Disconnect()
			close(disconnected)
		}()
		select {
		case <-disconnected:
		case <-time.After(5 * time.Second):
			ul.Log.Warn().Msg("Client disconnection timed out")
		}
	}
}
