package moderation

import (
	"context"
	"errors"
	"fmt"
	"github.com/jinzhu/gorm"
	"github.com/jonas747/dcmd"
	"github.com/jonas747/discordgo"
	"github.com/jonas747/dutil/dstate"
	"github.com/jonas747/yagpdb/bot"
	"github.com/jonas747/yagpdb/bot/eventsystem"
	"github.com/jonas747/yagpdb/commands"
	"github.com/jonas747/yagpdb/common"
	"github.com/jonas747/yagpdb/common/pubsub"
	"github.com/jonas747/yagpdb/common/scheduledevents"
	"github.com/mediocregopher/radix.v3"
	"github.com/sirupsen/logrus"
	"regexp"
	"strings"
	"time"
)

var (
	ErrFailedPerms = errors.New("Failed retrieving perms")
)

type ContextKey int

const (
	ContextKeyConfig ContextKey = iota
)

const MuteDeniedChannelPerms = discordgo.PermissionSendMessages | discordgo.PermissionVoiceSpeak

var _ commands.CommandProvider = (*Plugin)(nil)
var _ bot.BotInitHandler = (*Plugin)(nil)
var _ bot.ShardMigrationHandler = (*Plugin)(nil)

func (p *Plugin) AddCommands() {
	commands.AddRootCommands(ModerationCommands...)
}

func (p *Plugin) BotInit() {
	scheduledevents.RegisterEventHandler("unmute", handleUnMute)
	scheduledevents.RegisterEventHandler("mod_unban", handleUnban)

	eventsystem.AddHandler(HandleGuildBanAddRemove, eventsystem.EventGuildBanAdd, eventsystem.EventGuildBanRemove)
	eventsystem.AddHandler(LockMemberMuteMW(HandleMemberJoin), eventsystem.EventGuildMemberAdd)
	eventsystem.AddHandler(LockMemberMuteMW(HandleGuildMemberUpdate), eventsystem.EventGuildMemberUpdate)

	eventsystem.AddHandler(bot.ConcurrentEventHandler(HandleGuildCreate), eventsystem.EventGuildCreate)
	eventsystem.AddHandler(HandleChannelCreateUpdate, eventsystem.EventChannelUpdate, eventsystem.EventChannelUpdate)

	pubsub.AddHandler("mod_refresh_mute_override", HandleRefreshMuteOverrides, nil)
}

func (p *Plugin) GuildMigrated(gs *dstate.GuildState, toThisSlave bool) {
	if !toThisSlave {
		return
	}

	go RefreshMuteOverrides(gs.ID)
}

func HandleRefreshMuteOverrides(evt *pubsub.Event) {
	RefreshMuteOverrides(evt.TargetGuildInt)
}

func HandleGuildCreate(evt *eventsystem.EventData) {
	gc := evt.GuildCreate()
	RefreshMuteOverrides(gc.ID)
}

// Refreshes the mute override on the channel, currently it only adds it.
func RefreshMuteOverrides(guildID int64) {

	config, err := GetConfig(guildID)
	if err != nil {
		return
	}

	if config.MuteRole == "" || !config.MuteManageRole {
		return
	}

	guild := bot.State.Guild(true, guildID)
	if guild == nil {
		return // Still starting up and haven't received the guild yet
	}

	guild.RLock()
	defer guild.RUnlock()
	for _, v := range guild.Channels {
		RefreshMuteOverrideForChannel(config, v.DGoCopy())
	}
}

func HandleChannelCreateUpdate(evt *eventsystem.EventData) {
	var channel *discordgo.Channel
	if evt.Type == eventsystem.EventChannelCreate {
		channel = evt.ChannelCreate().Channel
	} else {
		channel = evt.ChannelUpdate().Channel
	}

	if channel.GuildID == 0 {
		return
	}

	config, err := GetConfig(channel.GuildID)
	if err != nil {
		return
	}

	if config.MuteRole == "" || !config.MuteManageRole {
		return
	}

	RefreshMuteOverrideForChannel(config, channel)
}

func RefreshMuteOverrideForChannel(config *Config, channel *discordgo.Channel) {
	// Ignore the channel
	if common.ContainsInt64Slice(config.MuteIgnoreChannels, channel.ID) {
		return
	}

	var override *discordgo.PermissionOverwrite

	// Check for existing override
	for _, v := range channel.PermissionOverwrites {
		if v.Type == "role" && v.ID == config.IntMuteRole() {
			override = v
			break
		}
	}

	allows := 0
	denies := MuteDeniedChannelPerms
	changed := true

	if override != nil {
		allows = override.Allow
		denies = override.Deny
		changed = false

		if (allows & MuteDeniedChannelPerms) != 0 {
			// One of the mute permissions was in the allows, remove it
			allows &= ^MuteDeniedChannelPerms
			changed = true
		}

		if (denies & MuteDeniedChannelPerms) != MuteDeniedChannelPerms {
			// Missing one of the mute permissions
			denies |= MuteDeniedChannelPerms
			changed = true
		}
	}

	if changed {
		go common.BotSession.ChannelPermissionSet(channel.ID, config.IntMuteRole(), "role", allows, denies)
	}
}

func HandleGuildBanAddRemove(evt *eventsystem.EventData) {
	var user *discordgo.User
	var guildID int64
	var action ModlogAction

	botPerformed := false

	switch evt.Type {
	case eventsystem.EventGuildBanAdd:

		guildID = evt.GuildBanAdd().GuildID
		user = evt.GuildBanAdd().User
		action = MABanned

		var i int
		common.RedisPool.Do(radix.Cmd(&i, "GET", RedisKeyBannedUser(guildID, user.ID)))
		if i > 0 {
			// The bot banned the user earlier, don't make duplicate entries in the modlog
			common.RedisPool.Do(radix.Cmd(nil, "DEL", RedisKeyBannedUser(guildID, user.ID)))
			return
		}

	case eventsystem.EventGuildBanRemove:
		action = MAUnbanned
		user = evt.GuildBanRemove().User
		guildID = evt.GuildBanRemove().GuildID

		var i int
		common.RedisPool.Do(radix.Cmd(&i, "GET", RedisKeyUnbannedUser(guildID, user.ID)))
		if i > 0 {
			// The bot was the one that performed the unban
			common.RedisPool.Do(radix.Cmd(nil, "DEL", RedisKeyUnbannedUser(guildID, user.ID)))
			botPerformed = true
		}

	default:
		return
	}

	config, err := GetConfig(guildID)
	if err != nil {
		logrus.WithError(err).WithField("guild", guildID).Error("Failed retrieving config")
		return
	}

	if config.ActionChannel == "" {
		// No modlog channel set up
		return
	}

	if (action == MAUnbanned && !config.LogUnbans && !botPerformed) ||
		(action == MABanned && !config.LogBans) {
		return
	}

	var author *discordgo.User
	reason := ""
	if botPerformed {
		author = common.BotUser
		reason = "Timed ban expired"
	}

	err = CreateModlogEmbed(config.IntActionChannel(), author, action, user, reason, "")
	if err != nil {
		logrus.WithError(err).WithField("guild", guildID).Error("Failed sending " + action.Prefix + " log message")
	}
}

// Since updating mutes are now a complex operation with removing roles and whatnot,
// to avoid weird bugs from happening we lock it so it can only be updated one place per user
func LockMemberMuteMW(next func(evt *eventsystem.EventData)) func(evt *eventsystem.EventData) {
	return func(evt *eventsystem.EventData) {
		var userID int64
		var guild int64
		// TODO: add utility functions to the eventdata struct for fetching things like these?
		if evt.Type == eventsystem.EventGuildMemberAdd {
			userID = evt.GuildMemberAdd().User.ID
			guild = evt.GuildMemberAdd().GuildID
		} else if evt.Type == eventsystem.EventGuildMemberUpdate {
			userID = evt.GuildMemberUpdate().User.ID
			guild = evt.GuildMemberUpdate().GuildID
		} else {
			panic("Unknown event in lock memebr mute middleware")
		}

		// If there's less than 2 seconds of the mute left, don't bother doing anything
		var muteLeft int
		common.RedisPool.Do(radix.Cmd(&muteLeft, "TTL", RedisKeyMutedUser(guild, userID)))
		if muteLeft < 5 {
			return
		}

		LockMute(userID)
		defer UnlockMute(userID)

		// The situation may have changed at th is point, check again

		// muteLeft, _ = client.Cmd("TTL", RedisKeyMutedUser(guild, userID)).Int()
		common.RedisPool.Do(radix.Cmd(&muteLeft, "TTL", RedisKeyMutedUser(guild, userID)))
		if muteLeft < 5 {
			return
		}

		next(evt)
	}
}

func HandleMemberJoin(evt *eventsystem.EventData) {
	c := evt.GuildMemberAdd()

	config, err := GetConfig(c.GuildID)
	if err != nil {
		logrus.WithError(err).WithField("guild", c.GuildID).Error("Failed retrieving config")
		return
	}
	if config.MuteRole == "" {
		return
	}

	logrus.WithField("guild", c.GuildID).WithField("user", c.User.ID).Info("Assigning back mute role after member rejoined")
	err = common.BotSession.GuildMemberRoleAdd(c.GuildID, c.User.ID, config.IntMuteRole())
	if err != nil {
		logrus.WithField("guild", c.GuildID).WithError(err).Error("Failed assigning mute role")
	}
}

func HandleGuildMemberUpdate(evt *eventsystem.EventData) {
	c := evt.GuildMemberUpdate()

	config, err := GetConfig(c.GuildID)
	if err != nil {
		logrus.WithError(err).WithField("guild", c.GuildID).Error("Failed retrieving config")
		return
	}
	if config.MuteRole == "" {
		return
	}

	logrus.WithField("guild", c.Member.GuildID).WithField("user", c.User.ID).Info("Giving back mute roles arr")

	removedRoles, err := AddMemberMuteRole(config, c.Member.User.ID, c.Member.Roles)
	if err != nil {
		logrus.WithError(err).Error("Failed adding mute role to user in member update")
	}

	if len(removedRoles) < 1 {
		return
	}

	tx, err := common.PQ.Begin()
	if err != nil {
		logrus.WithError(err).Error("Failed starting transaction")
		return
	}

	// Append the removed roles to the removed_roles array column, if they don't already exist in it
	const queryStr = "UPDATE muted_users SET removed_roles = array_append(removed_roles, $3 ) WHERE user_id=$2 AND guild_id=$1 AND NOT ($3 = ANY(removed_roles));"
	for _, v := range removedRoles {
		_, err := tx.Exec(queryStr, c.GuildID, c.Member.User.ID, v)
		if err != nil {
			logrus.WithError(err).Error("Failed updating removed roles")
			break
		}
	}

	err = tx.Commit()
	if err != nil {
		logrus.WithError(err).Error("Failed comitting transaction")
	}
}

const (
	ModCmdBan int = iota
	ModCmdKick
	ModCmdMute
	ModCmdUnMute
	ModCmdClean
	ModCmdReport
	ModCmdReason
	ModCmdWarn
)

// ModBaseCmd is the base command for moderation commands, it makes sure proper permissions are there for the user invoking it
// and that the command is required and the reason is specified if required
func ModBaseCmd(neededPerm, cmd int, inner dcmd.RunFunc) dcmd.RunFunc {
	// userID, channelID, guildID string (config *Config, hasPerms bool, err error) {

	return func(data *dcmd.Data) (interface{}, error) {

		userID := data.Msg.Author.ID
		channelID := data.CS.ID
		guildID := data.GS.ID

		cmdName := data.Cmd.Trigger.Names[0]

		config, err := GetConfig(guildID)
		if err != nil {
			return "Error retrieving config", err
		}

		enabled := false
		reasonOptional := false
		var requiredRoles []int64

		reasonArgIndex := 1
		switch cmd {
		case ModCmdBan:
			enabled = config.BanEnabled
			reasonOptional = config.BanReasonOptional
			requiredRoles = config.BanCmdRoles
		case ModCmdKick:
			enabled = config.KickEnabled
			reasonOptional = config.KickReasonOptional
			requiredRoles = config.KickCmdRoles
		case ModCmdMute, ModCmdUnMute:
			enabled = config.MuteEnabled
			if cmd == ModCmdMute {
				reasonOptional = config.MuteReasonOptional
				reasonArgIndex = 2
			} else {
				reasonOptional = config.UnmuteReasonOptional
			}
			requiredRoles = config.MuteCmdRoles
		case ModCmdClean:
			reasonOptional = true
			enabled = config.CleanEnabled
			reasonArgIndex = -1
		case ModCmdReport:
			reasonOptional = true
			enabled = config.ReportEnabled
		case ModCmdReason:
			reasonOptional = true
			enabled = true
		case ModCmdWarn:
			reasonOptional = true
			enabled = config.WarnCommandsEnabled
			requiredRoles = config.WarnCmdRoles
		default:
			panic("Unknown command")
		}

		requiredRoleFound := false
		if len(requiredRoles) > 0 {
			// Check if the user has one of the required roles
			member, err := bot.GetMember(guildID, userID)
			if err != nil {
				return "Failed fetching member", err
			}
			for _, r := range member.Roles {
				if common.ContainsInt64Slice(requiredRoles, r) {
					requiredRoleFound = true
					break
				}
			}
		}

		if !requiredRoleFound && neededPerm != 0 {
			// Fallback to legacy permissions
			hasPerms, err := bot.AdminOrPerm(neededPerm, userID, channelID)
			if err != nil || !hasPerms {
				return fmt.Sprintf("The **%s** command requires the **%s** permission in this channel, you don't have it. (if you do contact bot support)", cmdName, common.StringPerms[neededPerm]), nil
			}
		}

		if !enabled {
			return fmt.Sprintf("The **%s** command is disabled on this server. Enable it in the control panel on the moderation page.", cmdName), nil
		}

		if reasonArgIndex != -1 && reasonArgIndex < len(data.Args) {
			reason := SafeArgString(data, reasonArgIndex)
			if !reasonOptional && reason == "" {
				return "Reason is required.", nil
			} else if reason == "" {
				data.Args[reasonArgIndex].Value = "(No reason specified)"
			}
		}

		return inner(data.WithContext(context.WithValue(data.Context(), ContextKeyConfig, config)))
	}
}

func SafeArgString(data *dcmd.Data, arg int) string {
	if arg >= len(data.Args) || data.Args[arg].Value == nil {
		return ""
	}

	return data.Args[arg].Str()
}

var ModerationCommands = []*commands.YAGCommand{
	&commands.YAGCommand{
		CustomEnabled: true,
		CmdCategory:   commands.CategoryModeration,
		Name:          "Ban",
		Description:   "Bans a member, specify a duration with -d",
		RequiredArgs:  1,
		Arguments: []*dcmd.ArgDef{
			&dcmd.ArgDef{Name: "User", Type: dcmd.UserReqMention},
			&dcmd.ArgDef{Name: "Reason", Type: dcmd.String},
		},
		ArgSwitches: []*dcmd.ArgDef{
			&dcmd.ArgDef{Switch: "d", Default: time.Duration(0), Name: "Duration", Type: &commands.DurationArg{}},
		},
		RunFunc: ModBaseCmd(discordgo.PermissionBanMembers, ModCmdBan, func(parsed *dcmd.Data) (interface{}, error) {
			config := parsed.Context().Value(ContextKeyConfig).(*Config)

			reason := SafeArgString(parsed, 1)

			target := parsed.Args[0].Value.(*discordgo.User)

			err := BanUserWithDuration(config, parsed.GS.ID, parsed.Msg.ChannelID, parsed.Msg.Author, reason, target, parsed.Switches["d"].Value.(time.Duration), true)
			if err != nil {
				if cast, ok := err.(*discordgo.RESTError); ok && cast.Message != nil {
					return cast.Message.Message, err
				} else {
					return "An error occurred", err
				}
			}

			return "👌", nil
		}),
	},
	&commands.YAGCommand{
		CustomEnabled: true,
		CmdCategory:   commands.CategoryModeration,
		Name:          "Banid",
		Description:   "Bans a user by id, specify a duration with -d",
		RequiredArgs:  1,
		Arguments: []*dcmd.ArgDef{
			&dcmd.ArgDef{Name: "User", Type: dcmd.Int},
			&dcmd.ArgDef{Name: "Reason", Type: dcmd.String},
		},
		ArgSwitches: []*dcmd.ArgDef{
			&dcmd.ArgDef{Switch: "d", Default: time.Duration(0), Name: "Duration", Type: &commands.DurationArg{}},
		},
		RunFunc: ModBaseCmd(discordgo.PermissionBanMembers, ModCmdBan, func(parsed *dcmd.Data) (interface{}, error) {
			config := parsed.Context().Value(ContextKeyConfig).(*Config)

			reason := SafeArgString(parsed, 1)

			targetID := parsed.Args[0].Int64()
			targetMember := parsed.GS.MemberCopy(true, targetID)
			var target *discordgo.User
			if targetMember == nil || !targetMember.MemberSet {
				target = &discordgo.User{
					Username:      "unknown",
					Discriminator: "????",
					ID:            targetID,
				}
			} else {
				target = targetMember.DGoUser()
			}

			err := BanUserWithDuration(config, parsed.GS.ID, parsed.Msg.ChannelID, parsed.Msg.Author, reason, target, parsed.Switches["d"].Value.(time.Duration), false)
			if err != nil {
				if cast, ok := err.(*discordgo.RESTError); ok && cast.Message != nil {
					return cast.Message.Message, err
				} else {
					return "An error occurred", err
				}
			}

			return "👌", nil
		}),
	},
	&commands.YAGCommand{
		CustomEnabled: true,
		CmdCategory:   commands.CategoryModeration,
		Name:          "Kick",
		Description:   "Kicks a member",
		RequiredArgs:  1,
		Arguments: []*dcmd.ArgDef{
			&dcmd.ArgDef{Name: "User", Type: dcmd.UserReqMention},
			&dcmd.ArgDef{Name: "Reason", Type: dcmd.String},
		},
		RunFunc: ModBaseCmd(discordgo.PermissionKickMembers, ModCmdKick, func(parsed *dcmd.Data) (interface{}, error) {
			config := parsed.Context().Value(ContextKeyConfig).(*Config)

			reason := SafeArgString(parsed, 1)

			target := parsed.Args[0].Value.(*discordgo.User)

			err := KickUser(config, parsed.GS.ID, parsed.Msg.ChannelID, parsed.Msg.Author, reason, target)
			if err != nil {
				if cast, ok := err.(*discordgo.RESTError); ok && cast.Message != nil {
					return cast.Message.Message, err
				} else {
					return "An error occurred", err
				}
			}

			return "👌", nil
		}),
	},
	&commands.YAGCommand{
		CustomEnabled: true,
		CmdCategory:   commands.CategoryModeration,
		Name:          "Mute",
		Description:   "Mutes a member",
		Arguments: []*dcmd.ArgDef{
			&dcmd.ArgDef{Name: "User", Type: dcmd.UserReqMention},
			&dcmd.ArgDef{Name: "Minutes", Default: 10, Type: &dcmd.IntArg{Min: 1, Max: 1440}},
			&dcmd.ArgDef{Name: "Reason", Type: dcmd.String},
		},
		ArgumentCombos: [][]int{[]int{0, 1, 2}, []int{0, 1}, []int{0, 2}, []int{0}},
		RunFunc: ModBaseCmd(discordgo.PermissionKickMembers, ModCmdMute, func(parsed *dcmd.Data) (interface{}, error) {
			config := parsed.Context().Value(ContextKeyConfig).(*Config)
			if config.MuteRole == "" {
				return "No mute role set up, assign a mute role in the control panel", nil
			}

			target := parsed.Args[0].Value.(*discordgo.User)
			muteDuration := parsed.Args[1].Int()
			reason := parsed.Args[2].Str()

			member, err := bot.GetMember(parsed.GS.ID, target.ID)
			if err != nil || member == nil {
				return "Member not found", err
			}

			err = MuteUnmuteUser(config, true, parsed.GS.ID, parsed.Msg.ChannelID, parsed.Msg.Author, reason, member, muteDuration)
			if err != nil {
				if cast, ok := err.(*discordgo.RESTError); ok && cast.Message != nil {
					return "API Error: " + cast.Message.Message, err
				} else {
					return "An error occurred", err
				}
			}

			return "👌", nil
		}),
	},
	&commands.YAGCommand{
		CustomEnabled: true,
		CmdCategory:   commands.CategoryModeration,
		Name:          "Unmute",
		Description:   "Unmutes a member",
		RequiredArgs:  1,
		Arguments: []*dcmd.ArgDef{
			&dcmd.ArgDef{Name: "User", Type: dcmd.UserReqMention},
			&dcmd.ArgDef{Name: "Reason", Type: dcmd.String},
		},
		RunFunc: ModBaseCmd(discordgo.PermissionKickMembers, ModCmdUnMute, func(parsed *dcmd.Data) (interface{}, error) {
			config := parsed.Context().Value(ContextKeyConfig).(*Config)
			if config.MuteRole == "" {
				return "No mute role set up, assign a mute role in the control panel", nil
			}

			target := parsed.Args[0].Value.(*discordgo.User)
			reason := parsed.Args[1].Str()

			member, err := bot.GetMember(parsed.GS.ID, target.ID)
			if err != nil || member == nil {
				return "Member not found", err
			}

			err = MuteUnmuteUser(config, false, parsed.GS.ID, parsed.Msg.ChannelID, parsed.Msg.Author, reason, member, 0)
			if err != nil {
				if cast, ok := err.(*discordgo.RESTError); ok && cast.Message != nil {
					return "API Error: " + cast.Message.Message, err
				} else {
					return "An error occurred", err
				}
			}

			return "👌", nil
		}),
	},
	&commands.YAGCommand{
		CustomEnabled: true,
		Cooldown:      5,
		CmdCategory:   commands.CategoryModeration,
		Name:          "Report",
		Description:   "Reports a member to the server's staff",
		RequiredArgs:  2,
		Arguments: []*dcmd.ArgDef{
			&dcmd.ArgDef{Name: "User", Type: dcmd.UserReqMention},
			&dcmd.ArgDef{Name: "Reason", Type: dcmd.String},
		},
		RunFunc: ModBaseCmd(0, ModCmdReport, func(parsed *dcmd.Data) (interface{}, error) {
			config := parsed.Context().Value(ContextKeyConfig).(*Config)

			logLink := CreateLogs(parsed.GS.ID, parsed.CS.ID, parsed.Msg.Author)

			channelID := config.IntReportChannel()
			if channelID == 0 {
				return "No report channel set up", nil
			}

			reportBody := fmt.Sprintf("<@%d> Reported <@%d> in <#%d> For `%s`\nLast 100 messages from channel: <%s>", parsed.Msg.Author.ID, parsed.Args[0].Value.(*discordgo.User).ID, parsed.Msg.ChannelID, parsed.Args[1].Str(), logLink)

			_, err := common.BotSession.ChannelMessageSend(channelID, common.EscapeSpecialMentions(reportBody))
			if err != nil {
				return "Failed sending report, check perms for report channel", err
			}

			// don't bother sending confirmation if it's in the same channel
			if channelID != parsed.Msg.ChannelID {
				return "User reported to the proper authorities", nil
			}
			return "👌", nil
		}),
	},
	&commands.YAGCommand{
		CustomEnabled:   true,
		CmdCategory:     commands.CategoryModeration,
		Name:            "Clean",
		Description:     "Delete the last number of messages from chat, optionally filtering by user, max age and regex.",
		LongDescription: "Specify a regex with \"-r regex_here\" and max age with \"-ma 1h10m\"\nNote: Will only look in the last 1k messages",
		Aliases:         []string{"clear", "cl"},
		RequiredArgs:    1,
		Arguments: []*dcmd.ArgDef{
			&dcmd.ArgDef{Name: "Num", Type: &dcmd.IntArg{Min: 1, Max: 100}},
			&dcmd.ArgDef{Name: "User", Type: dcmd.UserReqMention},
		},
		ArgSwitches: []*dcmd.ArgDef{
			&dcmd.ArgDef{Switch: "r", Name: "Regex", Type: dcmd.String},
			&dcmd.ArgDef{Switch: "ma", Default: time.Duration(0), Name: "Max age", Type: &commands.DurationArg{}},
			&dcmd.ArgDef{Switch: "i", Name: "Regex case insensitive"},
		},
		ArgumentCombos: [][]int{[]int{0}, []int{0, 1}, []int{1, 0}},
		RunFunc: ModBaseCmd(discordgo.PermissionManageMessages, ModCmdClean, func(parsed *dcmd.Data) (interface{}, error) {
			var userFilter int64
			if parsed.Args[1].Value != nil {
				userFilter = parsed.Args[1].Value.(*discordgo.User).ID
			}

			num := parsed.Args[0].Int()
			if userFilter == 0 || userFilter == parsed.Msg.Author.ID {
				num++ // Automatically include our own message
			}

			if num > 100 {
				num = 100
			}

			if num < 1 {
				if num < 0 {
					return errors.New("Bot is having a stroke <https://www.youtube.com/watch?v=dQw4w9WgXcQ>"), nil
				}
				return errors.New("Can't delete nothing"), nil
			}

			filtered := false

			// Check if we should regex match this
			re := ""
			if parsed.Switches["r"].Value != nil {
				filtered = true
				re = parsed.Switches["r"].Str()

				// Add the case insensitive flag if needed
				if parsed.Switches["i"].Value != nil && parsed.Switches["i"].Value.(bool) {
					if !strings.HasPrefix(re, "(?i)") {
						re = "(?i)" + re
					}
				}
			}

			// Check if we have a max age
			ma := parsed.Switches["ma"].Value.(time.Duration)
			if ma != 0 {
				filtered = true
			}

			limitFetch := num
			if userFilter != 0 || filtered {
				limitFetch = num * 50 // Maybe just change to full fetch?
			}

			if limitFetch > 1000 {
				limitFetch = 1000
			}

			// Wait a second so the client dosen't gltich out
			time.Sleep(time.Second)

			numDeleted, err := AdvancedDeleteMessages(parsed.Msg.ChannelID, userFilter, re, ma, num, limitFetch)

			return dcmd.NewTemporaryResponse(time.Second*5, fmt.Sprintf("Deleted %d message(s)! :')", numDeleted), true), err
		}),
	},
	&commands.YAGCommand{
		CustomEnabled: true,
		CmdCategory:   commands.CategoryModeration,
		Name:          "Reason",
		Description:   "Add/Edit a modlog reason",
		RequiredArgs:  2,
		Arguments: []*dcmd.ArgDef{
			&dcmd.ArgDef{Name: "ID", Type: dcmd.Int},
			&dcmd.ArgDef{Name: "Reason", Type: dcmd.String},
		},
		RunFunc: ModBaseCmd(discordgo.PermissionKickMembers, ModCmdReason, func(parsed *dcmd.Data) (interface{}, error) {
			config := parsed.Context().Value(ContextKeyConfig).(*Config)
			if config.ActionChannel == "" {
				return "No mod log channel set up", nil
			}
			msg, err := common.BotSession.ChannelMessage(config.IntActionChannel(), parsed.Args[0].Int64())
			if err != nil {
				if cast, ok := err.(*discordgo.RESTError); ok && cast.Message != nil {
					return "Failed retrieving the message: " + cast.Message.Message, nil
				}
				return "Failed retrieving the message", err
			}

			if msg.Author.ID != common.Conf.BotID {
				return "I didn't make that message", nil
			}

			if len(msg.Embeds) < 1 {
				return "This entry is either too old or you're trying to mess with me...", nil
			}

			embed := msg.Embeds[0]
			updateEmbedReason(parsed.Msg.Author, parsed.Args[1].Str(), embed)
			_, err = common.BotSession.ChannelMessageEditEmbed(config.IntActionChannel(), msg.ID, embed)
			if err != nil {
				return "Failed updating the modlog entry", err
			}

			return "👌", nil
		}),
	},
	&commands.YAGCommand{
		CustomEnabled: true,
		CmdCategory:   commands.CategoryModeration,
		Name:          "Warn",
		Description:   "Warns a user, warnings are saved using the bot. Use -warnings to view them.",
		RequiredArgs:  2,
		Arguments: []*dcmd.ArgDef{
			&dcmd.ArgDef{Name: "User", Type: dcmd.UserReqMention},
			&dcmd.ArgDef{Name: "Reason", Type: dcmd.String},
		},
		RunFunc: ModBaseCmd(discordgo.PermissionManageMessages, ModCmdWarn, func(parsed *dcmd.Data) (interface{}, error) {
			config := parsed.Context().Value(ContextKeyConfig).(*Config)

			err := WarnUser(config, parsed.GS.ID, parsed.CS.ID, parsed.Msg.Author, parsed.Args[0].Value.(*discordgo.User), parsed.Args[1].Str())
			if err != nil {
				return "Seomthing went wrong warning this user, make sure the bot has all the proper perms. (if you have the modlog enabled the bot need to be able to send messages in the modlog for example)", err
			}

			return "👌", nil
		}),
	},
	&commands.YAGCommand{
		CustomEnabled: true,
		CmdCategory:   commands.CategoryModeration,
		Name:          "Warnings",
		Description:   "Lists warning of a user.",
		Aliases:       []string{"Warns"},
		RequiredArgs:  1,
		Arguments: []*dcmd.ArgDef{
			&dcmd.ArgDef{Name: "User", Type: dcmd.UserReqMention},
		},
		RunFunc: ModBaseCmd(discordgo.PermissionManageMessages, ModCmdWarn, func(parsed *dcmd.Data) (interface{}, error) {
			var result []*WarningModel
			err := common.GORM.Where("user_id = ? AND guild_id = ?", parsed.Args[0].Value.(*discordgo.User).ID, parsed.GS.ID).Order("id desc").Find(&result).Error
			if err != nil && err != gorm.ErrRecordNotFound {
				return "An error occured...", err
			}

			if len(result) < 1 {
				return "This user has not received any warnings", nil
			}

			out := ""
			for _, entry := range result {
				out += fmt.Sprintf("#%d: `%20s` **%s** (%13s) - **%s**\n", entry.ID, entry.CreatedAt.Format(time.RFC822), entry.AuthorUsernameDiscrim, entry.AuthorID, entry.Message)
				if entry.LogsLink != "" {
					out += "^logs: <" + entry.LogsLink + ">\n"
				}
			}

			return out, nil
		}),
	},
	&commands.YAGCommand{
		CustomEnabled: true,
		CmdCategory:   commands.CategoryModeration,
		Name:          "EditWarning",
		Description:   "Edit a warning, id is the first number of each warning from the warnings command",
		RequiredArgs:  2,
		Arguments: []*dcmd.ArgDef{
			&dcmd.ArgDef{Name: "Id", Type: dcmd.Int},
			&dcmd.ArgDef{Name: "NewMessage", Type: dcmd.String},
		},
		RunFunc: ModBaseCmd(discordgo.PermissionManageMessages, ModCmdWarn, func(parsed *dcmd.Data) (interface{}, error) {

			rows := common.GORM.Model(WarningModel{}).Where("guild_id = ? AND id = ?", parsed.GS.ID, parsed.Args[0].Int()).Update(
				"message", fmt.Sprintf("%s (updated by %s#%s (%d))", parsed.Args[1].Str(), parsed.Msg.Author.Username, parsed.Msg.Author.Discriminator, parsed.Msg.Author.ID)).RowsAffected

			if rows < 1 {
				return "Failed updating, most likely couldn't find the warning", nil
			}

			return "👌", nil
		}),
	},
	&commands.YAGCommand{
		CustomEnabled: true,
		CmdCategory:   commands.CategoryModeration,
		Name:          "DelWarning",
		Aliases:       []string{"dw"},
		Description:   "Deletes a warning, id is the first number of each warning from the warnings command",
		RequiredArgs:  1,
		Arguments: []*dcmd.ArgDef{
			&dcmd.ArgDef{Name: "Id", Type: dcmd.Int},
		},
		RunFunc: ModBaseCmd(discordgo.PermissionManageMessages, ModCmdWarn, func(parsed *dcmd.Data) (interface{}, error) {

			rows := common.GORM.Where("guild_id = ? AND id = ?", parsed.GS.ID, parsed.Args[0].Int()).Delete(WarningModel{}).RowsAffected
			if rows < 1 {
				return "Failed deleting, most likely couldn't find the warning", nil
			}

			return "👌", nil
		}),
	},
	&commands.YAGCommand{
		CustomEnabled: true,
		CmdCategory:   commands.CategoryModeration,
		Name:          "ClearWarnings",
		Aliases:       []string{"clw"},
		Description:   "Clears the warnings of a user",
		RequiredArgs:  1,
		Arguments: []*dcmd.ArgDef{
			&dcmd.ArgDef{Name: "User", Type: dcmd.UserReqMention},
		},
		RunFunc: ModBaseCmd(discordgo.PermissionManageMessages, ModCmdWarn, func(parsed *dcmd.Data) (interface{}, error) {

			rows := common.GORM.Where("guild_id = ? AND user_id = ?", parsed.GS.ID, parsed.Args[0].Value.(*discordgo.User).ID).Delete(WarningModel{}).RowsAffected
			return fmt.Sprintf("Deleted %d warnings.", rows), nil
		}),
	},
}

func AdvancedDeleteMessages(channelID int64, filterUser int64, regex string, maxAge time.Duration, deleteNum, fetchNum int) (int, error) {
	var compiledRegex *regexp.Regexp
	if regex != "" {
		// Start by compiling the regex
		var err error
		compiledRegex, err = regexp.Compile(regex)
		if err != nil {
			return 0, err
		}
	}

	msgs, err := bot.GetMessages(channelID, fetchNum, false)
	if err != nil {
		return 0, err
	}

	toDelete := make([]int64, 0)
	now := time.Now()
	for i := len(msgs) - 1; i >= 0; i-- {
		if filterUser != 0 && msgs[i].Author.ID != filterUser {
			continue
		}

		parsedCreatedAt, _ := msgs[i].Timestamp.Parse()
		// Can only bulk delete messages up to 2 weeks (but add 1 minute buffer account for time sync issues and other smallies)
		if now.Sub(parsedCreatedAt) > (time.Hour*24*14)-time.Minute {
			continue
		}

		// Check regex
		if compiledRegex != nil {
			if !compiledRegex.MatchString(msgs[i].Content) {
				continue
			}
		}

		// Check max age
		if maxAge != 0 && now.Sub(parsedCreatedAt) > maxAge {
			continue
		}

		toDelete = append(toDelete, msgs[i].ID)
		//log.Println("Deleting", msgs[i].ContentWithMentionsReplaced())
		if len(toDelete) >= deleteNum || len(toDelete) >= 100 {
			break
		}
	}

	if len(toDelete) < 1 {
		return 0, nil
	}

	if len(toDelete) < 1 {
		return 0, nil
	} else if len(toDelete) == 1 {
		err = common.BotSession.ChannelMessageDelete(channelID, toDelete[0])
	} else {
		err = common.BotSession.ChannelMessagesBulkDelete(channelID, toDelete)
	}

	return len(toDelete), err
}
