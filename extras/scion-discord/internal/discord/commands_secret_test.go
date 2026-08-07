package discord

import (
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
	"github.com/stretchr/testify/assert"
)

// ---------------------------------------------------------------------------
// validateSecretKey
// ---------------------------------------------------------------------------

func TestValidateSecretKey_Valid(t *testing.T) {
	tests := []string{
		"MY_API_KEY",
		"simple",
		"DB_HOST",
		"a",
		"key-with-dashes",
		"key.with.dots",
	}
	for _, key := range tests {
		t.Run(key, func(t *testing.T) {
			assert.NoError(t, validateSecretKey(key))
		})
	}
}

func TestValidateSecretKey_Invalid(t *testing.T) {
	tests := []struct {
		name string
		key  string
	}{
		{"empty", ""},
		{"space", "has space"},
		{"tab", "has\ttab"},
		{"newline", "has\nnewline"},
		{"carriage_return", "has\rreturn"},
		{"equals", "key=value"},
		{"colon", "service:api_key"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateSecretKey(tt.key)
			assert.Error(t, err)
		})
	}
}

// ---------------------------------------------------------------------------
// getSubSubcommandOption
// ---------------------------------------------------------------------------

func TestGetSubSubcommandOption_ExtractsKey(t *testing.T) {
	i := &discordgo.InteractionCreate{
		Interaction: &discordgo.Interaction{
			Type: discordgo.InteractionApplicationCommand,
			Data: discordgo.ApplicationCommandInteractionData{
				Name: "scion",
				Options: []*discordgo.ApplicationCommandInteractionDataOption{
					{
						Name: "secret",
						Type: discordgo.ApplicationCommandOptionSubCommandGroup,
						Options: []*discordgo.ApplicationCommandInteractionDataOption{
							{
								Name: "set",
								Type: discordgo.ApplicationCommandOptionSubCommand,
								Options: []*discordgo.ApplicationCommandInteractionDataOption{
									{
										Name:  "key",
										Type:  discordgo.ApplicationCommandOptionString,
										Value: "MY_API_KEY",
									},
								},
							},
						},
					},
				},
			},
		},
	}

	got := getSubSubcommandOption(i, "key")
	assert.Equal(t, "MY_API_KEY", got)
}

func TestGetSubSubcommandOption_EmptyWhenNoOptions(t *testing.T) {
	i := &discordgo.InteractionCreate{
		Interaction: &discordgo.Interaction{
			Type: discordgo.InteractionApplicationCommand,
			Data: discordgo.ApplicationCommandInteractionData{
				Name:    "scion",
				Options: []*discordgo.ApplicationCommandInteractionDataOption{},
			},
		},
	}

	got := getSubSubcommandOption(i, "key")
	assert.Empty(t, got)
}

func TestGetSubSubcommandOption_EmptyWhenNoSubSub(t *testing.T) {
	i := &discordgo.InteractionCreate{
		Interaction: &discordgo.Interaction{
			Type: discordgo.InteractionApplicationCommand,
			Data: discordgo.ApplicationCommandInteractionData{
				Name: "scion",
				Options: []*discordgo.ApplicationCommandInteractionDataOption{
					{
						Name:    "secret",
						Type:    discordgo.ApplicationCommandOptionSubCommandGroup,
						Options: []*discordgo.ApplicationCommandInteractionDataOption{},
					},
				},
			},
		},
	}

	got := getSubSubcommandOption(i, "key")
	assert.Empty(t, got)
}

func TestGetSubSubcommandOption_WrongOptionName(t *testing.T) {
	i := &discordgo.InteractionCreate{
		Interaction: &discordgo.Interaction{
			Type: discordgo.InteractionApplicationCommand,
			Data: discordgo.ApplicationCommandInteractionData{
				Name: "scion",
				Options: []*discordgo.ApplicationCommandInteractionDataOption{
					{
						Name: "secret",
						Type: discordgo.ApplicationCommandOptionSubCommandGroup,
						Options: []*discordgo.ApplicationCommandInteractionDataOption{
							{
								Name: "get",
								Type: discordgo.ApplicationCommandOptionSubCommand,
								Options: []*discordgo.ApplicationCommandInteractionDataOption{
									{
										Name:  "key",
										Type:  discordgo.ApplicationCommandOptionString,
										Value: "DB_HOST",
									},
								},
							},
						},
					},
				},
			},
		},
	}

	got := getSubSubcommandOption(i, "nonexistent")
	assert.Empty(t, got)

	got = getSubSubcommandOption(i, "key")
	assert.Equal(t, "DB_HOST", got)
}

// ---------------------------------------------------------------------------
// ephemeralCommands map includes "secret"
// ---------------------------------------------------------------------------

func TestEphemeralCommands_IncludesSecret(t *testing.T) {
	assert.True(t, ephemeralCommands["secret"],
		"secret commands should be in the ephemeral commands map")
}

// ---------------------------------------------------------------------------
// helpText includes secret commands
// ---------------------------------------------------------------------------

func TestHelpText_IncludesSecretCommands(t *testing.T) {
	text := helpText()
	assert.Contains(t, text, "/scion secret list")
	assert.Contains(t, text, "/scion secret set")
	assert.Contains(t, text, "/scion secret get")
	assert.Contains(t, text, "/scion secret delete")
}

// ---------------------------------------------------------------------------
// Modal submit customID parsing
// ---------------------------------------------------------------------------

func TestSecretModalCustomID_ParsesCorrectly(t *testing.T) {
	customID := "secret:set:mykey:proj-uuid-1234"
	parts := strings.SplitN(customID, ":", 4)

	assert.Len(t, parts, 4)
	assert.Equal(t, "secret", parts[0])
	assert.Equal(t, "set", parts[1])
	assert.Equal(t, "mykey", parts[2])
	assert.Equal(t, "proj-uuid-1234", parts[3])
}

func TestSecretModalCustomID_ColonInKeyRejected(t *testing.T) {
	// After R1 fix, keys with colons are rejected before they
	// reach the customID construction, preventing mis-parsing.
	err := validateSecretKey("service:api_key")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "':'")
}
