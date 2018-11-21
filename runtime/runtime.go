// Copyright 2018 The Nakama Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

/*
Package runtime is an API to interact with the embedded Runtime environment in Nakama.

The game server includes support to develop native code in Go with the plugin package from the Go stdlib.
It's used to enable compiled shared objects to be loaded by the game server at startup.

The Go runtime support can be used to develop authoritative multiplayer match handlers,
RPC functions, hook into messages processed by the server, and extend the server withany other custom logic.
It offers the same capabilities as the Lua runtime support but has the advantage that any package from the Go ecosystem can be used.

Here's the smallest example of a Go module written with the server runtime.

	package main

	import (
		"context"
		"database/sql"
		"log"

		"github.com/heroiclabs/nakama/runtime"
	)

	func InitModule(ctx context.Context, logger *log.Logger, db *sql.DB, nk runtime.NakamaModule, initializer runtime.Initializer) error {
		if err := initializer.RegisterRpc("get_time", getServerTime); err != nil {
			return err
		}
		logger.Println("module loaded")
		return nil
	}

	func getServerTime(ctx context.Context, logger *log.Logger, db *sql.DB, nk runtime.NakamaModule, payload string) (string, error) {
		serverTime := map[string]int64 {
			"time": time.Now().UTC().Unix(),
		}

		response, err := json.Marshal(serverTime)
		if err != nil {
			logger.Printf("failed to marshal response: %v", response)
			return "", errors.New("internal error; see logs")
		}
		return string(response), nil
	}

On server start, Nakama scans the module directory folder (https://heroiclabs.com/docs/runtime-code-basics/#load-modules).
If it finds a shared object file (*.so), it attempts to open the file as a plugin and initialize it by running the InitModule function.
This function is guaranteed to ever be invoked once during the uptime of the server.

To setup your own project to build modules for the game server you can follow these steps.

1. Build Nakama from source:
	go get -d github.com/heroiclabs/nakama
	cd $GOPATH/src/github.com/heroiclabs/nakama
	env CGO_ENABLED=1 go build

2. Setup a folder for your own server code:
	mkdir -p $GOPATH/src/some_project
	cd $GOPATH/src/some_project

3. Build your plugin as a shared object:
	go build --buildmode=plugin -o ./modules/some_project.so

NOTE: It is not possible to build plugins on Windows with the native compiler toolchain but they can be cross-compiled and run with Docker.

4. Start Nakama with your module:
	$GOPATH/src/github.com/heroiclabs/nakama/nakama --runtime.path $GOPATH/src/plugin_project/modules

TIP: You don't have to install Nakama from source but you still need to have the `api`, `rtapi` and `runtime` packages from Nakama on your `GOPATH`. Heroic Labs also offers a docker plugin-builder image that streamlines the plugin workflow.

For more information about the Go runtime have a look at the docs:
https://heroiclabs.com/docs/runtime-code-basics
*/
package runtime

import (
	"context"
	"database/sql"
	"log"

	"github.com/heroiclabs/nakama/api"
	"github.com/heroiclabs/nakama/rtapi"
)

const (
	// All available environmental variables made available to the runtime environment.
	// This is useful to store API keys and other secrets which may be different between servers run in production and in development.
	//   envs := ctx.Value(runtime.RUNTIME_CTX_ENV).(map[string]string)
	// This can always be safely cast into a `map[string]string`.
	RUNTIME_CTX_ENV = "env"

	// The mode associated with the execution context. It's one of these values:
	//  "run_once", "rpc", "before", "after", "match", "matchmaker", "leaderboard_reset", "tournament_reset", "tournament_end".
	RUNTIME_CTX_MODE = "execution_mode"

	// Query params that was passed through from HTTP request.
	RUNTIME_CTX_QUERY_PARAMS = "query_params"

	// The user ID associated with the execution context.
	RUNTIME_CTX_USER_ID = "user_id"

	// The username associated with the execution context.
	RUNTIME_CTX_USERNAME = "username"

	// The user session expiry in seconds associated with the execution context.
	RUNTIME_CTX_USER_SESSION_EXP = "user_session_exp"

	// The user session associated with the execution context.
	RUNTIME_CTX_SESSION_ID = "session_id"

	// The IP address of the client making the request.
	RUNTIME_CTX_CLIENT_IP = "client_ip"

	// The port number of the client making the request.
	RUNTIME_CTX_CLIENT_PORT = "client_port"

	// The match ID that is currently being executed. Only applicable to server authoritative multiplayer.
	RUNTIME_CTX_MATCH_ID = "match_id"

	// The node ID that the match is being executed on. Only applicable to server authoritative multiplayer.
	RUNTIME_CTX_MATCH_NODE = "match_node"

	// Labels associated with the match. Only applicable to server authoritative multiplayer.
	RUNTIME_CTX_MATCH_LABEL = "match_label"

	// Tick rate defined for this match. Only applicable to server authoritative multiplayer.
	RUNTIME_CTX_MATCH_TICK_RATE = "match_tick_rate"
)

/*
Error is used to indicate a failure in code. The message and code are returned to the client.
If an Error is used as response for a HTTP/gRPC request, then the server tries to use the error value as the gRPC error code. This will in turn translate to HTTP status codes.

For more information, please have a look at the following:
	https://github.com/grpc/grpc-go/blob/master/codes/codes.go
	https://github.com/grpc-ecosystem/grpc-gateway/blob/master/runtime/errors.go
	https://golang.org/pkg/net/http/
*/
type Error struct {
	Message string
	Code    int
}

// Error returns the encapsulated error message.
func (e *Error) Error() string {
	return e.Message
}

/*
NewError returns a new error. The message and code are sent directly to the client. The code field is also optionally translated to gRPC/HTTP code.
	runtime.NewError("Server unavailable", 14) // 14 = Unavailable = 503 HTTP status code
*/
func NewError(message string, code int) *Error {
	return &Error{Message: message, Code: code}
}

/*
Initializer is used to register various callback functions with the server.
It is made available to the InitModule function as an input parameter when the function is invoked by the server when loading the module on server start.

NOTE: You must not cache the reference to this and reuse it as a later point as this could have unintended side effects.
*/
type Initializer interface {

	/*
		RegisterRpc registers a function with the given ID. This ID can be used within client code to send an RPC message to
		execute the function and return the result. Results are always returned as a JSON string (or optionally empty string).

		If there is an issue with the RPC call, return an empty string and the associated error which will be returned to the client.
	*/
	RegisterRpc(id string, fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, payload string) (string, error)) error

	/*
		RegisterBeforeRt registers a function for a message. The registered function will be called after the message has been processed in the pipeline.
		The custom code will be executed asynchronously after the response message has been sent to a client

		Message names can be found here: https://heroiclabs.com/docs/runtime-code-basics/#message-names
	*/
	RegisterBeforeRt(id string, fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, envelope *rtapi.Envelope) (*rtapi.Envelope, error)) error

	/*
		RegisterAfterRt registers a function with for a message. Any function may be registered to intercept a message received from a client and operate on it (or reject it) based on custom logic.
		This is useful to enforce specific rules on top of the standard features in the server.

		You can return `nil` instead of the `rtapi.Envelope` and this will disable disable that particular server functionality.

		Message names can be found here: https://heroiclabs.com/docs/runtime-code-basics/#message-names
	*/
	RegisterAfterRt(id string, fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, envelope *rtapi.Envelope) error) error

	// RegisterBeforeGetAccount is used to register a function invoked when the server receives the relevant request.
	RegisterBeforeGetAccount(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule) error) error

	// RegisterAfterGetAccount is used to register a function invoked after the server processes the relevant request.
	RegisterAfterGetAccount(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, out *api.Account) error) error

	// RegisterBeforeUpdateAccount is used to register a function invoked when the server receives the relevant request.
	RegisterBeforeUpdateAccount(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, in *api.UpdateAccountRequest) (*api.UpdateAccountRequest, error)) error

	// RegisterAfterUpdateAccount is used to register a function invoked after the server processes the relevant request.
	RegisterAfterUpdateAccount(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, in *api.UpdateAccountRequest) error) error

	// RegisterBeforeAuthenticateCustom can be used to perform pre-authentication checks.
	// You can use this to process the input (such as decoding custom tokens) and ensure inter-compatibility between Nakama and your own custom system.
	RegisterBeforeAuthenticateCustom(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, in *api.AuthenticateCustomRequest) (*api.AuthenticateCustomRequest, error)) error

	// RegisterAfterAuthenticateCustom can be used to perform after successful authentication checks.
	// For instance, you can run special logic if the account was just created like adding them to newcomers leaderboard.
	RegisterAfterAuthenticateCustom(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, out *api.Session, in *api.AuthenticateCustomRequest) error) error

	// RegisterBeforeAuthenticateDevice can be used to perform pre-authentication checks.
	RegisterBeforeAuthenticateDevice(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, in *api.AuthenticateDeviceRequest) (*api.AuthenticateDeviceRequest, error)) error

	// RegisterAfterAuthenticateDevice can be used to perform after successful authentication checks.
	RegisterAfterAuthenticateDevice(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, out *api.Session, in *api.AuthenticateDeviceRequest) error) error

	// RegisterBeforeAuthenticateEmail can be used to perform pre-authentication checks.
	RegisterBeforeAuthenticateEmail(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, in *api.AuthenticateEmailRequest) (*api.AuthenticateEmailRequest, error)) error

	// RegisterAfterAuthenticateEmail can be used to perform after successful authentication checks.
	RegisterAfterAuthenticateEmail(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, out *api.Session, in *api.AuthenticateEmailRequest) error) error

	// RegisterBeforeAuthenticateFacebook can be used to perform pre-authentication checks.
	RegisterBeforeAuthenticateFacebook(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, in *api.AuthenticateFacebookRequest) (*api.AuthenticateFacebookRequest, error)) error

	// RegisterAfterAuthenticateFacebook can be used to perform after successful authentication checks.
	RegisterAfterAuthenticateFacebook(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, out *api.Session, in *api.AuthenticateFacebookRequest) error) error

	// RegisterBeforeAuthenticateGameCenter can be used to perform pre-authentication checks.
	RegisterBeforeAuthenticateGameCenter(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, in *api.AuthenticateGameCenterRequest) (*api.AuthenticateGameCenterRequest, error)) error

	// RegisterAfterAuthenticateGameCenter can be used to perform after successful authentication checks.
	RegisterAfterAuthenticateGameCenter(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, out *api.Session, in *api.AuthenticateGameCenterRequest) error) error

	// RegisterBeforeAuthenticateGoogle can be used to perform pre-authentication checks.
	RegisterBeforeAuthenticateGoogle(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, in *api.AuthenticateGoogleRequest) (*api.AuthenticateGoogleRequest, error)) error

	// RegisterAfterAuthenticateGoogle can be used to perform after successful authentication checks.
	RegisterAfterAuthenticateGoogle(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, out *api.Session, in *api.AuthenticateGoogleRequest) error) error

	// RegisterBeforeAuthenticateSteam can be used to perform pre-authentication checks.
	RegisterBeforeAuthenticateSteam(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, in *api.AuthenticateSteamRequest) (*api.AuthenticateSteamRequest, error)) error

	// RegisterAfterAuthenticateSteam can be used to perform after successful authentication checks.
	RegisterAfterAuthenticateSteam(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, out *api.Session, in *api.AuthenticateSteamRequest) error) error

	RegisterBeforeListChannelMessages(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, in *api.ListChannelMessagesRequest) (*api.ListChannelMessagesRequest, error)) error
	RegisterAfterListChannelMessages(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, out *api.ChannelMessageList, in *api.ListChannelMessagesRequest) error) error
	RegisterBeforeListFriends(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule) error) error
	RegisterAfterListFriends(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, out *api.Friends) error) error
	RegisterBeforeAddFriends(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, in *api.AddFriendsRequest) (*api.AddFriendsRequest, error)) error
	RegisterAfterAddFriends(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, in *api.AddFriendsRequest) error) error
	RegisterBeforeDeleteFriends(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, in *api.DeleteFriendsRequest) (*api.DeleteFriendsRequest, error)) error
	RegisterAfterDeleteFriends(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, in *api.DeleteFriendsRequest) error) error
	RegisterBeforeBlockFriends(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, in *api.BlockFriendsRequest) (*api.BlockFriendsRequest, error)) error
	RegisterAfterBlockFriends(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, in *api.BlockFriendsRequest) error) error
	RegisterBeforeImportFacebookFriends(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, in *api.ImportFacebookFriendsRequest) (*api.ImportFacebookFriendsRequest, error)) error
	RegisterAfterImportFacebookFriends(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, in *api.ImportFacebookFriendsRequest) error) error
	RegisterBeforeCreateGroup(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, in *api.CreateGroupRequest) (*api.CreateGroupRequest, error)) error
	RegisterAfterCreateGroup(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, out *api.Group, in *api.CreateGroupRequest) error) error
	RegisterBeforeUpdateGroup(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, in *api.UpdateGroupRequest) (*api.UpdateGroupRequest, error)) error
	RegisterAfterUpdateGroup(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, in *api.UpdateGroupRequest) error) error
	RegisterBeforeDeleteGroup(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, in *api.DeleteGroupRequest) (*api.DeleteGroupRequest, error)) error
	RegisterAfterDeleteGroup(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, in *api.DeleteGroupRequest) error) error
	RegisterBeforeJoinGroup(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, in *api.JoinGroupRequest) (*api.JoinGroupRequest, error)) error
	RegisterAfterJoinGroup(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, in *api.JoinGroupRequest) error) error
	RegisterBeforeLeaveGroup(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, in *api.LeaveGroupRequest) (*api.LeaveGroupRequest, error)) error
	RegisterAfterLeaveGroup(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, in *api.LeaveGroupRequest) error) error
	RegisterBeforeAddGroupUsers(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, in *api.AddGroupUsersRequest) (*api.AddGroupUsersRequest, error)) error
	RegisterAfterAddGroupUsers(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, in *api.AddGroupUsersRequest) error) error
	RegisterBeforeKickGroupUsers(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, in *api.KickGroupUsersRequest) (*api.KickGroupUsersRequest, error)) error
	RegisterAfterKickGroupUsers(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, in *api.KickGroupUsersRequest) error) error
	RegisterBeforePromoteGroupUsers(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, in *api.PromoteGroupUsersRequest) (*api.PromoteGroupUsersRequest, error)) error
	RegisterAfterPromoteGroupUsers(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, in *api.PromoteGroupUsersRequest) error) error
	RegisterBeforeListGroupUsers(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, in *api.ListGroupUsersRequest) (*api.ListGroupUsersRequest, error)) error
	RegisterAfterListGroupUsers(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, out *api.GroupUserList, in *api.ListGroupUsersRequest) error) error
	RegisterBeforeListUserGroups(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, in *api.ListUserGroupsRequest) (*api.ListUserGroupsRequest, error)) error
	RegisterAfterListUserGroups(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, out *api.UserGroupList, in *api.ListUserGroupsRequest) error) error
	RegisterBeforeListGroups(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, in *api.ListGroupsRequest) (*api.ListGroupsRequest, error)) error
	RegisterAfterListGroups(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, out *api.GroupList, in *api.ListGroupsRequest) error) error
	RegisterBeforeDeleteLeaderboardRecord(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, in *api.DeleteLeaderboardRecordRequest) (*api.DeleteLeaderboardRecordRequest, error)) error
	RegisterAfterDeleteLeaderboardRecord(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, in *api.DeleteLeaderboardRecordRequest) error) error
	RegisterBeforeListLeaderboardRecords(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, in *api.ListLeaderboardRecordsRequest) (*api.ListLeaderboardRecordsRequest, error)) error
	RegisterAfterListLeaderboardRecords(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, out *api.LeaderboardRecordList, in *api.ListLeaderboardRecordsRequest) error) error
	RegisterBeforeWriteLeaderboardRecord(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, in *api.WriteLeaderboardRecordRequest) (*api.WriteLeaderboardRecordRequest, error)) error
	RegisterAfterWriteLeaderboardRecord(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, out *api.LeaderboardRecord, in *api.WriteLeaderboardRecordRequest) error) error
	RegisterBeforeListLeaderboardRecordsAroundOwner(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, in *api.ListLeaderboardRecordsAroundOwnerRequest) (*api.ListLeaderboardRecordsAroundOwnerRequest, error)) error
	RegisterAfterListLeaderboardRecordsAroundOwner(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, out *api.LeaderboardRecordList, in *api.ListLeaderboardRecordsAroundOwnerRequest) error) error
	RegisterBeforeLinkCustom(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, in *api.AccountCustom) (*api.AccountCustom, error)) error
	RegisterAfterLinkCustom(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, in *api.AccountCustom) error) error
	RegisterBeforeLinkDevice(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, in *api.AccountDevice) (*api.AccountDevice, error)) error
	RegisterAfterLinkDevice(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, in *api.AccountDevice) error) error
	RegisterBeforeLinkEmail(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, in *api.AccountEmail) (*api.AccountEmail, error)) error
	RegisterAfterLinkEmail(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, in *api.AccountEmail) error) error
	RegisterBeforeLinkFacebook(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, in *api.LinkFacebookRequest) (*api.LinkFacebookRequest, error)) error
	RegisterAfterLinkFacebook(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, in *api.LinkFacebookRequest) error) error
	RegisterBeforeLinkGameCenter(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, in *api.AccountGameCenter) (*api.AccountGameCenter, error)) error
	RegisterAfterLinkGameCenter(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, in *api.AccountGameCenter) error) error
	RegisterBeforeLinkGoogle(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, in *api.AccountGoogle) (*api.AccountGoogle, error)) error
	RegisterAfterLinkGoogle(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, in *api.AccountGoogle) error) error
	RegisterBeforeLinkSteam(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, in *api.AccountSteam) (*api.AccountSteam, error)) error
	RegisterAfterLinkSteam(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, in *api.AccountSteam) error) error
	RegisterBeforeListMatches(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, in *api.ListMatchesRequest) (*api.ListMatchesRequest, error)) error
	RegisterAfterListMatches(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, out *api.MatchList, in *api.ListMatchesRequest) error) error
	RegisterBeforeListNotifications(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, in *api.ListNotificationsRequest) (*api.ListNotificationsRequest, error)) error
	RegisterAfterListNotifications(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, out *api.NotificationList, in *api.ListNotificationsRequest) error) error
	RegisterBeforeDeleteNotification(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, in *api.DeleteNotificationsRequest) (*api.DeleteNotificationsRequest, error)) error
	RegisterAfterDeleteNotification(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, in *api.DeleteNotificationsRequest) error) error
	RegisterBeforeListStorageObjects(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, in *api.ListStorageObjectsRequest) (*api.ListStorageObjectsRequest, error)) error
	RegisterAfterListStorageObjects(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, out *api.StorageObjectList, in *api.ListStorageObjectsRequest) error) error
	RegisterBeforeReadStorageObjects(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, in *api.ReadStorageObjectsRequest) (*api.ReadStorageObjectsRequest, error)) error
	RegisterAfterReadStorageObjects(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, out *api.StorageObjects, in *api.ReadStorageObjectsRequest) error) error
	RegisterBeforeWriteStorageObjects(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, in *api.WriteStorageObjectsRequest) (*api.WriteStorageObjectsRequest, error)) error
	RegisterAfterWriteStorageObjects(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, out *api.StorageObjectAcks, in *api.WriteStorageObjectsRequest) error) error
	RegisterBeforeDeleteStorageObjects(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, in *api.DeleteStorageObjectsRequest) (*api.DeleteStorageObjectsRequest, error)) error
	RegisterAfterDeleteStorageObjects(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, in *api.DeleteStorageObjectsRequest) error) error
	RegisterBeforeJoinTournament(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, in *api.JoinTournamentRequest) (*api.JoinTournamentRequest, error)) error
	RegisterAfterJoinTournament(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, in *api.JoinTournamentRequest) error) error
	RegisterBeforeListTournamentRecords(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, in *api.ListTournamentRecordsRequest) (*api.ListTournamentRecordsRequest, error)) error
	RegisterAfterListTournamentRecords(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, out *api.TournamentRecordList, in *api.ListTournamentRecordsRequest) error) error
	RegisterBeforeListTournaments(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, in *api.ListTournamentsRequest) (*api.ListTournamentsRequest, error)) error
	RegisterAfterListTournaments(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, out *api.TournamentList, in *api.ListTournamentsRequest) error) error
	RegisterBeforeWriteTournamentRecord(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, in *api.WriteTournamentRecordRequest) (*api.WriteTournamentRecordRequest, error)) error
	RegisterAfterWriteTournamentRecord(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, out *api.LeaderboardRecord, in *api.WriteTournamentRecordRequest) error) error
	RegisterBeforeListTournamentRecordsAroundOwner(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, in *api.ListTournamentRecordsAroundOwnerRequest) (*api.ListTournamentRecordsAroundOwnerRequest, error)) error
	RegisterAfterListTournamentRecordsAroundOwner(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, out *api.TournamentRecordList, in *api.ListTournamentRecordsAroundOwnerRequest) error) error
	RegisterBeforeUnlinkCustom(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, in *api.AccountCustom) (*api.AccountCustom, error)) error
	RegisterAfterUnlinkCustom(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, in *api.AccountCustom) error) error
	RegisterBeforeUnlinkDevice(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, in *api.AccountDevice) (*api.AccountDevice, error)) error
	RegisterAfterUnlinkDevice(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, in *api.AccountDevice) error) error
	RegisterBeforeUnlinkEmail(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, in *api.AccountEmail) (*api.AccountEmail, error)) error
	RegisterAfterUnlinkEmail(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, in *api.AccountEmail) error) error
	RegisterBeforeUnlinkFacebook(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, in *api.AccountFacebook) (*api.AccountFacebook, error)) error
	RegisterAfterUnlinkFacebook(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, in *api.AccountFacebook) error) error
	RegisterBeforeUnlinkGameCenter(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, in *api.AccountGameCenter) (*api.AccountGameCenter, error)) error
	RegisterAfterUnlinkGameCenter(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, in *api.AccountGameCenter) error) error
	RegisterBeforeUnlinkGoogle(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, in *api.AccountGoogle) (*api.AccountGoogle, error)) error
	RegisterAfterUnlinkGoogle(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, in *api.AccountGoogle) error) error
	RegisterBeforeUnlinkSteam(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, in *api.AccountSteam) (*api.AccountSteam, error)) error
	RegisterAfterUnlinkSteam(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, in *api.AccountSteam) error) error
	RegisterBeforeGetUsers(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, in *api.GetUsersRequest) (*api.GetUsersRequest, error)) error
	RegisterAfterGetUsers(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, out *api.Users, in *api.GetUsersRequest) error) error

	RegisterMatchmakerMatched(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, entries []MatchmakerEntry) (string, error)) error

	RegisterMatch(name string, fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule) (Match, error)) error

	RegisterTournamentEnd(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, tournament *api.Tournament, end, reset int64) error) error
	RegisterTournamentReset(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, tournament *api.Tournament, end, reset int64) error) error

	RegisterLeaderboardReset(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, leaderboard Leaderboard, reset int64) error) error
}

type Leaderboard interface {
	GetId() string
	GetAuthoritative() bool
	GetSortOrder() string
	GetOperator() string
	GetReset() string
	GetMetadata() map[string]interface{}
	GetCreateTime() int64
}

type PresenceMeta interface {
	GetHidden() bool
	GetPersistence() bool
	GetUsername() string
	GetStatus() string
}

type Presence interface {
	PresenceMeta
	GetUserId() string
	GetSessionId() string
	GetNodeId() string
}

type MatchmakerEntry interface {
	GetPresence() Presence
	GetTicket() string
	GetProperties() map[string]interface{}
}

type MatchData interface {
	Presence
	GetOpCode() int64
	GetData() []byte
	GetReceiveTime() int64
}

type MatchDispatcher interface {
	BroadcastMessage(opCode int64, data []byte, presences []Presence, sender Presence) error
	MatchKick(presences []Presence) error
	MatchLabelUpdate(label string) error
}

type Match interface {
	MatchInit(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, params map[string]interface{}) (interface{}, int, string)
	MatchJoinAttempt(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, dispatcher MatchDispatcher, tick int64, state interface{}, presence Presence, metadata map[string]string) (interface{}, bool, string)
	MatchJoin(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, dispatcher MatchDispatcher, tick int64, state interface{}, presences []Presence) interface{}
	MatchLeave(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, dispatcher MatchDispatcher, tick int64, state interface{}, presences []Presence) interface{}
	MatchLoop(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, dispatcher MatchDispatcher, tick int64, state interface{}, messages []MatchData) interface{}
	MatchTerminate(ctx context.Context, logger *log.Logger, db *sql.DB, nk NakamaModule, dispatcher MatchDispatcher, tick int64, state interface{}, graceSeconds int) interface{}
}

type NotificationSend struct {
	UserID     string
	Subject    string
	Content    map[string]interface{}
	Code       int
	Sender     string
	Persistent bool
}

type WalletUpdate struct {
	UserID    string
	Changeset map[string]interface{}
	Metadata  map[string]interface{}
}

type WalletLedgerItem interface {
	GetID() string
	GetUserID() string
	GetCreateTime() int64
	GetUpdateTime() int64
	GetChangeset() map[string]interface{}
	GetMetadata() map[string]interface{}
}

type StorageRead struct {
	Collection string
	Key        string
	UserID     string
}

type StorageWrite struct {
	Collection      string
	Key             string
	UserID          string
	Value           string
	Version         string
	PermissionRead  int
	PermissionWrite int
}

type StorageDelete struct {
	Collection string
	Key        string
	UserID     string
	Version    string
}

type NakamaModule interface {
	AuthenticateCustom(ctx context.Context, id, username string, create bool) (string, string, bool, error)
	AuthenticateDevice(ctx context.Context, id, username string, create bool) (string, string, bool, error)
	AuthenticateEmail(ctx context.Context, email, password, username string, create bool) (string, string, bool, error)
	AuthenticateFacebook(ctx context.Context, token string, importFriends bool, username string, create bool) (string, string, bool, error)
	AuthenticateGameCenter(ctx context.Context, playerID, bundleID string, timestamp int64, salt, signature, publicKeyUrl, username string, create bool) (string, string, bool, error)
	AuthenticateGoogle(ctx context.Context, token, username string, create bool) (string, string, bool, error)
	AuthenticateSteam(ctx context.Context, token, username string, create bool) (string, string, bool, error)

	AuthenticateTokenGenerate(userID, username string, exp int64) (string, int64, error)

	AccountGetId(ctx context.Context, userID string) (*api.Account, error)
	AccountUpdateId(ctx context.Context, userID, username string, metadata map[string]interface{}, displayName, timezone, location, langTag, avatarUrl string) error

	UsersGetId(ctx context.Context, userIDs []string) ([]*api.User, error)
	UsersGetUsername(ctx context.Context, usernames []string) ([]*api.User, error)
	UsersBanId(ctx context.Context, userIDs []string) error
	UsersUnbanId(ctx context.Context, userIDs []string) error

	StreamUserList(mode uint8, subject, descriptor, label string, includeHidden, includeNotHidden bool) ([]Presence, error)
	StreamUserGet(mode uint8, subject, descriptor, label, userID, sessionID string) (PresenceMeta, error)
	StreamUserJoin(mode uint8, subject, descriptor, label, userID, sessionID string, hidden, persistence bool, status string) (bool, error)
	StreamUserUpdate(mode uint8, subject, descriptor, label, userID, sessionID string, hidden, persistence bool, status string) error
	StreamUserLeave(mode uint8, subject, descriptor, label, userID, sessionID string) error
	StreamCount(mode uint8, subject, descriptor, label string) (int, error)
	StreamClose(mode uint8, subject, descriptor, label string) error
	StreamSend(mode uint8, subject, descriptor, label, data string) error
	StreamSendRaw(mode uint8, subject, descriptor, label string, msg *rtapi.Envelope) error

	MatchCreate(ctx context.Context, module string, params map[string]interface{}) (string, error)
	MatchList(ctx context.Context, limit int, authoritative bool, label string, minSize, maxSize int, query string) ([]*api.Match, error)

	NotificationSend(ctx context.Context, userID, subject string, content map[string]interface{}, code int, sender string, persistent bool) error
	NotificationsSend(ctx context.Context, notifications []*NotificationSend) error

	WalletUpdate(ctx context.Context, userID string, changeset, metadata map[string]interface{}, updateLedger bool) error
	WalletsUpdate(ctx context.Context, updates []*WalletUpdate, updateLedger bool) error
	WalletLedgerUpdate(ctx context.Context, itemID string, metadata map[string]interface{}) (WalletLedgerItem, error)
	WalletLedgerList(ctx context.Context, userID string) ([]WalletLedgerItem, error)

	StorageList(ctx context.Context, userID, collection string, limit int, cursor string) ([]*api.StorageObject, string, error)
	StorageRead(ctx context.Context, reads []*StorageRead) ([]*api.StorageObject, error)
	StorageWrite(ctx context.Context, writes []*StorageWrite) ([]*api.StorageObjectAck, error)
	StorageDelete(ctx context.Context, deletes []*StorageDelete) error

	LeaderboardCreate(ctx context.Context, id string, authoritative bool, sortOrder, operator, resetSchedule string, metadata map[string]interface{}) error
	LeaderboardDelete(ctx context.Context, id string) error
	LeaderboardRecordsList(ctx context.Context, id string, ownerIDs []string, limit int, cursor string, expiry int64) ([]*api.LeaderboardRecord, []*api.LeaderboardRecord, string, string, error)
	LeaderboardRecordWrite(ctx context.Context, id, ownerID, username string, score, subscore int64, metadata map[string]interface{}) (*api.LeaderboardRecord, error)
	LeaderboardRecordDelete(ctx context.Context, id, ownerID string) error

	TournamentCreate(ctx context.Context, id string, sortOrder, operator, resetSchedule string, metadata map[string]interface{}, title, description string, category, startTime, endTime, duration, maxSize, maxNumScore int, joinRequired bool) error
	TournamentDelete(ctx context.Context, id string) error
	TournamentAddAttempt(ctx context.Context, id, ownerID string, count int) error
	TournamentJoin(ctx context.Context, id, ownerID, username string) error
	TournamentList(ctx context.Context, categoryStart, categoryEnd, startTime, endTime, limit int, cursor string) (*api.TournamentList, error)
	TournamentRecordWrite(ctx context.Context, id, ownerID, username string, score, subscore int64, metadata map[string]interface{}) (*api.LeaderboardRecord, error)
	TournamentRecordsHaystack(ctx context.Context, id, ownerID string, limit int) ([]*api.LeaderboardRecord, error)

	GroupsGetId(ctx context.Context, groupIDs []string) ([]*api.Group, error)
	GroupCreate(ctx context.Context, userID, name, creatorID, langTag, description, avatarUrl string, open bool, metadata map[string]interface{}, maxCount int) (*api.Group, error)
	GroupUpdate(ctx context.Context, id, name, creatorID, langTag, description, avatarUrl string, open bool, metadata map[string]interface{}, maxCount int) error
	GroupDelete(ctx context.Context, id string) error
	GroupUsersList(ctx context.Context, id string) ([]*api.GroupUserList_GroupUser, error)
	UserGroupsList(ctx context.Context, userID string) ([]*api.UserGroupList_UserGroup, error)
}
