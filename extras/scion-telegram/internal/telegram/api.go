// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"
)

const (
	// defaultBaseURL is the default Telegram Bot API base URL.
	defaultBaseURL = "https://api.telegram.org"

	// defaultTimeout is the default HTTP client timeout for non-polling requests.
	defaultTimeout = 10 * time.Second

	// longPollTimeout is the timeout for getUpdates long-polling requests.
	// The HTTP client timeout must be longer than this to allow the server
	// to hold the connection.
	longPollTimeout = 30

	// longPollHTTPTimeout adds headroom above the Telegram long-poll timeout
	// for network latency and server-side processing.
	longPollHTTPTimeout = 35 * time.Second
)

// BotUser represents a Telegram bot user returned by getMe.
type BotUser struct {
	ID                      int64  `json:"id"`
	IsBot                   bool   `json:"is_bot"`
	FirstName               string `json:"first_name"`
	Username                string `json:"username"`
	CanReadAllGroupMessages bool   `json:"can_read_all_group_messages"`
}

// Update represents a Telegram update from getUpdates.
type Update struct {
	UpdateID      int64          `json:"update_id"`
	Message       *TGMessage     `json:"message,omitempty"`
	CallbackQuery *CallbackQuery `json:"callback_query,omitempty"`
}

// PhotoSize represents one available size of a photo or file/sticker thumbnail.
type PhotoSize struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id"`
	Width        int    `json:"width"`
	Height       int    `json:"height"`
	FileSize     int64  `json:"file_size"`
}

// TGDocument represents a general file sent in a Telegram message.
type TGDocument struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id"`
	FileName     string `json:"file_name"`
	MimeType     string `json:"mime_type"`
	FileSize     int64  `json:"file_size"`
}

// TGFile represents a file ready for download, returned by the getFile API.
type TGFile struct {
	FileID   string `json:"file_id"`
	FileSize int64  `json:"file_size"`
	FilePath string `json:"file_path"`
}

// TGMessage represents a Telegram message.
type TGMessage struct {
	MessageID       int64           `json:"message_id"`
	From            *TGUser         `json:"from,omitempty"`
	Chat            TGChat          `json:"chat"`
	Date            int64           `json:"date"`
	Text            string          `json:"text"`
	Caption         string          `json:"caption,omitempty"`
	Entities        []MessageEntity `json:"entities,omitempty"`
	ReplyToMessage  *TGMessage      `json:"reply_to_message,omitempty"`
	MigrateToChatID int64           `json:"migrate_to_chat_id,omitempty"`
	Photo           []PhotoSize     `json:"photo,omitempty"`
	Document        *TGDocument     `json:"document,omitempty"`
}

// MessageEntity represents a special entity in a Telegram message (e.g. @mentions, commands).
type MessageEntity struct {
	Type   string  `json:"type"`
	Offset int     `json:"offset"`
	Length int     `json:"length"`
	User   *TGUser `json:"user,omitempty"`
}

// TGUser represents a Telegram user.
type TGUser struct {
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name,omitempty"`
	Username  string `json:"username,omitempty"`
}

// TGChat represents a Telegram chat.
type TGChat struct {
	ID    int64  `json:"id"`
	Type  string `json:"type"`
	Title string `json:"title,omitempty"`
}

// apiResponse is the generic Telegram Bot API response wrapper.
type apiResponse struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result,omitempty"`
	Description string          `json:"description,omitempty"`
	ErrorCode   int             `json:"error_code,omitempty"`
	Parameters  *apiParameters  `json:"parameters,omitempty"`
}

// apiParameters contains optional response parameters from the Telegram API,
// such as retry_after for 429 rate-limit responses.
type apiParameters struct {
	RetryAfterSec   int   `json:"retry_after,omitempty"`
	MigrateToChatID int64 `json:"migrate_to_chat_id,omitempty"`
}

// APIError represents a non-OK response from the Telegram Bot API.
type APIError struct {
	Code            int
	Description     string
	RetryAfterSec   int
	MigrateToChatID int64
}

func (e *APIError) Error() string {
	if e.RetryAfterSec > 0 {
		return fmt.Sprintf("telegram API error %d: %s (retry after %ds)", e.Code, e.Description, e.RetryAfterSec)
	}
	return fmt.Sprintf("telegram API error %d: %s", e.Code, e.Description)
}

// IsTransient returns true for errors that represent temporary failures
// (429 rate limits, 5xx server errors) rather than permanent ones.
func (e *APIError) IsTransient() bool {
	return e.Code == http.StatusTooManyRequests || e.Code >= 500
}

func (e *APIError) IsMigrated() bool {
	return e.MigrateToChatID != 0
}

// InlineKeyboardMarkup represents a Telegram inline keyboard attached to a message.
type InlineKeyboardMarkup struct {
	InlineKeyboard [][]InlineKeyboardButton `json:"inline_keyboard"`
}

// InlineKeyboardButton represents a single button in an inline keyboard.
type InlineKeyboardButton struct {
	Text         string `json:"text"`
	CallbackData string `json:"callback_data,omitempty"`
	URL          string `json:"url,omitempty"`
}

// CallbackQuery represents an incoming callback query from a button press.
type CallbackQuery struct {
	ID      string     `json:"id"`
	From    *TGUser    `json:"from"`
	Message *TGMessage `json:"message,omitempty"`
	Data    string     `json:"data,omitempty"`
}

// sendMessageRequest is the JSON body for the sendMessage API call.
type sendMessageRequest struct {
	ChatID    int64  `json:"chat_id"`
	Text      string `json:"text"`
	ParseMode string `json:"parse_mode,omitempty"`
}

// sendMessageWithKeyboardRequest is the JSON body for sendMessage with an inline keyboard.
type sendMessageWithKeyboardRequest struct {
	ChatID           int64                 `json:"chat_id"`
	Text             string                `json:"text"`
	ParseMode        string                `json:"parse_mode,omitempty"`
	ReplyMarkup      *InlineKeyboardMarkup `json:"reply_markup,omitempty"`
	ReplyToMessageID int64                 `json:"reply_to_message_id,omitempty"`
}

// ForceReply instructs Telegram clients to display a reply interface to the
// user, as if the user had selected the bot's message and tapped 'Reply'.
type ForceReply struct {
	ForceReply bool `json:"force_reply"`
	Selective  bool `json:"selective,omitempty"`
}

// sendMessageForceReplyRequest is the JSON body for sendMessage with
// ForceReply markup and an optional inline keyboard.
type sendMessageForceReplyRequest struct {
	ChatID    int64            `json:"chat_id"`
	Text      string           `json:"text"`
	ParseMode string           `json:"parse_mode,omitempty"`
	ReplyMarkup json.RawMessage `json:"reply_markup,omitempty"`
}

// editMessageTextRequest is the JSON body for the editMessageText API call.
type editMessageTextRequest struct {
	ChatID      int64                 `json:"chat_id"`
	MessageID   int64                 `json:"message_id"`
	Text        string                `json:"text"`
	ParseMode   string                `json:"parse_mode,omitempty"`
	ReplyMarkup *InlineKeyboardMarkup `json:"reply_markup,omitempty"`
}

// editMessageReplyMarkupRequest is the JSON body for the editMessageReplyMarkup API call.
type editMessageReplyMarkupRequest struct {
	ChatID      int64                 `json:"chat_id"`
	MessageID   int64                 `json:"message_id"`
	ReplyMarkup *InlineKeyboardMarkup `json:"reply_markup,omitempty"`
}

// answerCallbackQueryRequest is the JSON body for the answerCallbackQuery API call.
type answerCallbackQueryRequest struct {
	CallbackQueryID string `json:"callback_query_id"`
	Text            string `json:"text,omitempty"`
	ShowAlert       bool   `json:"show_alert,omitempty"`
}

// deleteMessageRequest is the JSON body for the deleteMessage API call.
type deleteMessageRequest struct {
	ChatID    int64 `json:"chat_id"`
	MessageID int64 `json:"message_id"`
}

// BotCommand represents a bot command for the setMyCommands API.
type BotCommand struct {
	Command     string `json:"command"`
	Description string `json:"description"`
}

// BotCommandScope specifies the scope of bot commands (e.g. private chats, group chats).
type BotCommandScope struct {
	Type string `json:"type"`
}

// setMyCommandsRequest is the JSON body for the setMyCommands API call.
type setMyCommandsRequest struct {
	Commands []BotCommand     `json:"commands"`
	Scope    *BotCommandScope `json:"scope,omitempty"`
}

// setWebhookRequest is the JSON body for the setWebhook API call.
type setWebhookRequest struct {
	URL            string   `json:"url"`
	SecretToken    string   `json:"secret_token,omitempty"`
	AllowedUpdates []string `json:"allowed_updates,omitempty"`
}

// getUpdatesRequest is the JSON body for the getUpdates API call.
type getUpdatesRequest struct {
	Offset         int64    `json:"offset,omitempty"`
	Timeout        int      `json:"timeout"`
	AllowedUpdates []string `json:"allowed_updates,omitempty"`
}

// TelegramAPIClient provides methods to interact with the Telegram Bot API.
type TelegramAPIClient struct {
	botToken   string
	baseURL    string
	httpClient *http.Client

	// pollClient is a separate HTTP client with a longer timeout for
	// long-polling getUpdates requests.
	pollClient *http.Client
}

// NewAPIClient creates a new Telegram API client.
// The baseURL parameter allows overriding the API URL for testing.
func NewAPIClient(botToken, baseURL string) *TelegramAPIClient {
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	return &TelegramAPIClient{
		botToken:   botToken,
		baseURL:    baseURL,
		httpClient: &http.Client{Timeout: defaultTimeout},
		pollClient: &http.Client{Timeout: longPollHTTPTimeout},
	}
}

// methodURL constructs the full URL for a Telegram Bot API method.
func (c *TelegramAPIClient) methodURL(method string) string {
	return fmt.Sprintf("%s/bot%s/%s", c.baseURL, c.botToken, method)
}

// redactToken removes the bot token from error messages to prevent
// accidental credential leakage in logs.
func (c *TelegramAPIClient) redactToken(err error) error {
	if err == nil || c.botToken == "" {
		return err
	}
	return fmt.Errorf("%s", strings.ReplaceAll(err.Error(), c.botToken, "[REDACTED]"))
}

// GetChat calls the getChat API to retrieve chat information including the title.
func (c *TelegramAPIClient) GetChat(ctx context.Context, chatID int64) (*TGChat, error) {
	url := fmt.Sprintf("%s?chat_id=%d", c.methodURL("getChat"), chatID)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("create getChat request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("getChat request failed: %w", c.redactToken(err))
	}
	defer resp.Body.Close()

	var apiResp apiResponse
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return nil, fmt.Errorf("decode getChat response: %w", err)
	}

	if !apiResp.OK {
		return nil, &APIError{Code: apiResp.ErrorCode, Description: apiResp.Description}
	}

	var chat TGChat
	if err := json.Unmarshal(apiResp.Result, &chat); err != nil {
		return nil, fmt.Errorf("unmarshal getChat result: %w", err)
	}

	return &chat, nil
}

// GetMe calls the getMe API to validate the bot token and retrieve bot info.
func (c *TelegramAPIClient) GetMe(ctx context.Context) (*BotUser, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", c.methodURL("getMe"), nil)
	if err != nil {
		return nil, fmt.Errorf("create getMe request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("getMe request failed: %w", c.redactToken(err))
	}
	defer resp.Body.Close()

	var apiResp apiResponse
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return nil, fmt.Errorf("decode getMe response: %w", err)
	}

	if !apiResp.OK {
		return nil, fmt.Errorf("getMe failed: %s (code %d)", apiResp.Description, apiResp.ErrorCode)
	}

	var bot BotUser
	if err := json.Unmarshal(apiResp.Result, &bot); err != nil {
		return nil, fmt.Errorf("unmarshal getMe result: %w", err)
	}

	return &bot, nil
}

// SetMyCommands registers the bot's command list with Telegram for autocomplete.
// If scope is non-nil, commands are set for that specific scope only.
func (c *TelegramAPIClient) SetMyCommands(ctx context.Context, commands []BotCommand, scope *BotCommandScope) error {
	body := setMyCommandsRequest{
		Commands: commands,
		Scope:    scope,
	}

	jsonBody, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal setMyCommands request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", c.methodURL("setMyCommands"), bytes.NewReader(jsonBody))
	if err != nil {
		return fmt.Errorf("create setMyCommands request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("setMyCommands request failed: %w", c.redactToken(err))
	}
	defer resp.Body.Close()

	var apiResp apiResponse
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return fmt.Errorf("decode setMyCommands response: %w", err)
	}

	if !apiResp.OK {
		return &APIError{Code: apiResp.ErrorCode, Description: apiResp.Description}
	}

	return nil
}

// GetUpdates calls the getUpdates API with long polling to receive updates.
// The offset parameter ensures updates are acknowledged (Telegram will not
// return updates with IDs less than offset).
func (c *TelegramAPIClient) GetUpdates(ctx context.Context, offset int64, timeout int) ([]Update, error) {
	body := getUpdatesRequest{
		Offset:         offset,
		Timeout:        timeout,
		AllowedUpdates: []string{"message"},
	}

	jsonBody, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal getUpdates request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", c.methodURL("getUpdates"), bytes.NewReader(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("create getUpdates request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.pollClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("getUpdates request failed: %w", c.redactToken(err))
	}
	defer resp.Body.Close()

	var apiResp apiResponse
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return nil, fmt.Errorf("decode getUpdates response: %w", err)
	}

	if !apiResp.OK {
		return nil, fmt.Errorf("getUpdates failed: %s (code %d)", apiResp.Description, apiResp.ErrorCode)
	}

	var updates []Update
	if err := json.Unmarshal(apiResp.Result, &updates); err != nil {
		return nil, fmt.Errorf("unmarshal getUpdates result: %w", err)
	}

	return updates, nil
}

// SendMessage sends a text message to the specified chat.
func (c *TelegramAPIClient) SendMessage(ctx context.Context, chatID int64, text, parseMode string) (*TGMessage, error) {
	body := sendMessageRequest{
		ChatID:    chatID,
		Text:      text,
		ParseMode: parseMode,
	}

	jsonBody, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal sendMessage request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", c.methodURL("sendMessage"), bytes.NewReader(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("create sendMessage request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("sendMessage request failed: %w", c.redactToken(err))
	}
	defer resp.Body.Close()

	var apiResp apiResponse
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return nil, fmt.Errorf("decode sendMessage response: %w", err)
	}

	if !apiResp.OK {
		apiErr := &APIError{Code: apiResp.ErrorCode, Description: apiResp.Description}
		if apiResp.Parameters != nil {
			apiErr.RetryAfterSec = apiResp.Parameters.RetryAfterSec
			apiErr.MigrateToChatID = apiResp.Parameters.MigrateToChatID
		}
		return nil, apiErr
	}

	var msg TGMessage
	if err := json.Unmarshal(apiResp.Result, &msg); err != nil {
		return nil, fmt.Errorf("unmarshal sendMessage result: %w", err)
	}

	return &msg, nil
}

// SendMessageWithKeyboard sends a text message with an inline keyboard and optional reply.
func (c *TelegramAPIClient) SendMessageWithKeyboard(ctx context.Context, chatID int64, text, parseMode string, keyboard *InlineKeyboardMarkup, replyToMessageID int64) (*TGMessage, error) {
	body := sendMessageWithKeyboardRequest{
		ChatID:           chatID,
		Text:             text,
		ParseMode:        parseMode,
		ReplyMarkup:      keyboard,
		ReplyToMessageID: replyToMessageID,
	}

	jsonBody, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal sendMessage request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", c.methodURL("sendMessage"), bytes.NewReader(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("create sendMessage request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("sendMessage request failed: %w", c.redactToken(err))
	}
	defer resp.Body.Close()

	var apiResp apiResponse
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return nil, fmt.Errorf("decode sendMessage response: %w", err)
	}

	if !apiResp.OK {
		apiErr := &APIError{Code: apiResp.ErrorCode, Description: apiResp.Description}
		if apiResp.Parameters != nil {
			apiErr.RetryAfterSec = apiResp.Parameters.RetryAfterSec
			apiErr.MigrateToChatID = apiResp.Parameters.MigrateToChatID
		}
		return nil, apiErr
	}

	var msg TGMessage
	if err := json.Unmarshal(apiResp.Result, &msg); err != nil {
		return nil, fmt.Errorf("unmarshal sendMessage result: %w", err)
	}

	return &msg, nil
}

// SendMessageWithForceReply sends a text message with Telegram's ForceReply
// markup so the recipient's client pre-focuses a reply input. If keyboard is
// non-nil, inline keyboard buttons are included instead of the ForceReply
// (Telegram only allows one reply_markup type per message).
func (c *TelegramAPIClient) SendMessageWithForceReply(ctx context.Context, chatID int64, text, parseMode string, keyboard *InlineKeyboardMarkup) (*TGMessage, error) {
	var markup json.RawMessage
	var err error
	if keyboard != nil {
		markup, err = json.Marshal(keyboard)
	} else {
		markup, err = json.Marshal(&ForceReply{ForceReply: true, Selective: true})
	}
	if err != nil {
		return nil, fmt.Errorf("marshal reply_markup: %w", err)
	}

	body := sendMessageForceReplyRequest{
		ChatID:      chatID,
		Text:        text,
		ParseMode:   parseMode,
		ReplyMarkup: markup,
	}

	jsonBody, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal sendMessage request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", c.methodURL("sendMessage"), bytes.NewReader(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("create sendMessage request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("sendMessage request failed: %w", c.redactToken(err))
	}
	defer resp.Body.Close()

	var apiResp apiResponse
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return nil, fmt.Errorf("decode sendMessage response: %w", err)
	}

	if !apiResp.OK {
		apiErr := &APIError{Code: apiResp.ErrorCode, Description: apiResp.Description}
		if apiResp.Parameters != nil {
			apiErr.RetryAfterSec = apiResp.Parameters.RetryAfterSec
			apiErr.MigrateToChatID = apiResp.Parameters.MigrateToChatID
		}
		return nil, apiErr
	}

	var msg TGMessage
	if err := json.Unmarshal(apiResp.Result, &msg); err != nil {
		return nil, fmt.Errorf("unmarshal sendMessage result: %w", err)
	}

	return &msg, nil
}

// SendDocument uploads a file via Telegram's sendDocument API using
// multipart/form-data. The caption is sent alongside the document.
//
// Limitation: this method receives an io.Reader, so the caller is responsible
// for opening the file. The current broker reads files from the local
// filesystem (telegram_attachment_path metadata). This works when the plugin
// runs on the same host as the agent with a shared volume mount. In a GKE
// environment with separate volume mounts, the plugin will not have local
// access to agent files — a future telegram_attachment_url metadata key
// should fetch the file from a URL (GCS signed URL or hub download endpoint)
// instead.
func (c *TelegramAPIClient) SendDocument(ctx context.Context, chatID int64, filename string, document io.Reader, caption, parseMode string) (*TGMessage, error) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)

	if err := writer.WriteField("chat_id", fmt.Sprintf("%d", chatID)); err != nil {
		return nil, fmt.Errorf("write chat_id field: %w", err)
	}
	if caption != "" {
		if err := writer.WriteField("caption", caption); err != nil {
			return nil, fmt.Errorf("write caption field: %w", err)
		}
	}
	if parseMode != "" {
		if err := writer.WriteField("parse_mode", parseMode); err != nil {
			return nil, fmt.Errorf("write parse_mode field: %w", err)
		}
	}

	part, err := writer.CreateFormFile("document", filename)
	if err != nil {
		return nil, fmt.Errorf("create document form file: %w", err)
	}
	if _, err := io.Copy(part, document); err != nil {
		return nil, fmt.Errorf("copy document data: %w", err)
	}

	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("close multipart writer: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", c.methodURL("sendDocument"), &body)
	if err != nil {
		return nil, fmt.Errorf("create sendDocument request: %w", err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("sendDocument request failed: %w", c.redactToken(err))
	}
	defer resp.Body.Close()

	var apiResp apiResponse
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return nil, fmt.Errorf("decode sendDocument response: %w", err)
	}

	if !apiResp.OK {
		apiErr := &APIError{Code: apiResp.ErrorCode, Description: apiResp.Description}
		if apiResp.Parameters != nil {
			apiErr.RetryAfterSec = apiResp.Parameters.RetryAfterSec
			apiErr.MigrateToChatID = apiResp.Parameters.MigrateToChatID
		}
		return nil, apiErr
	}

	var msg TGMessage
	if err := json.Unmarshal(apiResp.Result, &msg); err != nil {
		return nil, fmt.Errorf("unmarshal sendDocument result: %w", err)
	}

	return &msg, nil
}

// EditMessageText edits the text and optional keyboard of an existing message.
func (c *TelegramAPIClient) EditMessageText(ctx context.Context, chatID int64, messageID int64, text, parseMode string, keyboard *InlineKeyboardMarkup) (*TGMessage, error) {
	body := editMessageTextRequest{
		ChatID:      chatID,
		MessageID:   messageID,
		Text:        text,
		ParseMode:   parseMode,
		ReplyMarkup: keyboard,
	}

	jsonBody, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal editMessageText request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", c.methodURL("editMessageText"), bytes.NewReader(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("create editMessageText request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("editMessageText request failed: %w", c.redactToken(err))
	}
	defer resp.Body.Close()

	var apiResp apiResponse
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return nil, fmt.Errorf("decode editMessageText response: %w", err)
	}

	if !apiResp.OK {
		apiErr := &APIError{Code: apiResp.ErrorCode, Description: apiResp.Description}
		if apiResp.Parameters != nil {
			apiErr.RetryAfterSec = apiResp.Parameters.RetryAfterSec
			apiErr.MigrateToChatID = apiResp.Parameters.MigrateToChatID
		}
		return nil, apiErr
	}

	var msg TGMessage
	if err := json.Unmarshal(apiResp.Result, &msg); err != nil {
		return nil, fmt.Errorf("unmarshal editMessageText result: %w", err)
	}

	return &msg, nil
}

// EditMessageReplyMarkup edits only the inline keyboard of an existing message.
func (c *TelegramAPIClient) EditMessageReplyMarkup(ctx context.Context, chatID int64, messageID int64, keyboard *InlineKeyboardMarkup) (*TGMessage, error) {
	body := editMessageReplyMarkupRequest{
		ChatID:      chatID,
		MessageID:   messageID,
		ReplyMarkup: keyboard,
	}

	jsonBody, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal editMessageReplyMarkup request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", c.methodURL("editMessageReplyMarkup"), bytes.NewReader(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("create editMessageReplyMarkup request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("editMessageReplyMarkup request failed: %w", c.redactToken(err))
	}
	defer resp.Body.Close()

	var apiResp apiResponse
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return nil, fmt.Errorf("decode editMessageReplyMarkup response: %w", err)
	}

	if !apiResp.OK {
		apiErr := &APIError{Code: apiResp.ErrorCode, Description: apiResp.Description}
		if apiResp.Parameters != nil {
			apiErr.RetryAfterSec = apiResp.Parameters.RetryAfterSec
			apiErr.MigrateToChatID = apiResp.Parameters.MigrateToChatID
		}
		return nil, apiErr
	}

	var msg TGMessage
	if err := json.Unmarshal(apiResp.Result, &msg); err != nil {
		return nil, fmt.Errorf("unmarshal editMessageReplyMarkup result: %w", err)
	}

	return &msg, nil
}

// AnswerCallbackQuery sends an acknowledgement for a callback query from an inline button.
func (c *TelegramAPIClient) AnswerCallbackQuery(ctx context.Context, callbackQueryID, text string, showAlert bool) error {
	body := answerCallbackQueryRequest{
		CallbackQueryID: callbackQueryID,
		Text:            text,
		ShowAlert:       showAlert,
	}

	jsonBody, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal answerCallbackQuery request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", c.methodURL("answerCallbackQuery"), bytes.NewReader(jsonBody))
	if err != nil {
		return fmt.Errorf("create answerCallbackQuery request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("answerCallbackQuery request failed: %w", c.redactToken(err))
	}
	defer resp.Body.Close()

	var apiResp apiResponse
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return fmt.Errorf("decode answerCallbackQuery response: %w", err)
	}

	if !apiResp.OK {
		apiErr := &APIError{Code: apiResp.ErrorCode, Description: apiResp.Description}
		if apiResp.Parameters != nil {
			apiErr.RetryAfterSec = apiResp.Parameters.RetryAfterSec
			apiErr.MigrateToChatID = apiResp.Parameters.MigrateToChatID
		}
		return apiErr
	}

	return nil
}

// SetWebhook registers a webhook URL with Telegram for receiving updates.
// When set, Telegram will POST updates to the given URL instead of requiring
// long-polling via getUpdates.
func (c *TelegramAPIClient) SetWebhook(ctx context.Context, webhookURL, secretToken string) error {
	body := setWebhookRequest{
		URL:            webhookURL,
		SecretToken:    secretToken,
		AllowedUpdates: []string{"message", "callback_query"},
	}

	jsonBody, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal setWebhook request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", c.methodURL("setWebhook"), bytes.NewReader(jsonBody))
	if err != nil {
		return fmt.Errorf("create setWebhook request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("setWebhook request failed: %w", c.redactToken(err))
	}
	defer resp.Body.Close()

	var apiResp apiResponse
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return fmt.Errorf("decode setWebhook response: %w", err)
	}

	if !apiResp.OK {
		return &APIError{Code: apiResp.ErrorCode, Description: apiResp.Description}
	}

	return nil
}

// DeleteWebhook removes the current webhook integration, reverting to
// getUpdates long-polling mode.
func (c *TelegramAPIClient) DeleteWebhook(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, "POST", c.methodURL("deleteWebhook"), nil)
	if err != nil {
		return fmt.Errorf("create deleteWebhook request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("deleteWebhook request failed: %w", c.redactToken(err))
	}
	defer resp.Body.Close()

	var apiResp apiResponse
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return fmt.Errorf("decode deleteWebhook response: %w", err)
	}

	if !apiResp.OK {
		return &APIError{Code: apiResp.ErrorCode, Description: apiResp.Description}
	}

	return nil
}

// GetFile calls the getFile API to get the file path for downloading.
func (c *TelegramAPIClient) GetFile(ctx context.Context, fileID string) (*TGFile, error) {
	url := fmt.Sprintf("%s?file_id=%s", c.methodURL("getFile"), fileID)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("create getFile request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("getFile request failed: %w", c.redactToken(err))
	}
	defer resp.Body.Close()

	var apiResp apiResponse
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return nil, fmt.Errorf("decode getFile response: %w", err)
	}

	if !apiResp.OK {
		return nil, &APIError{Code: apiResp.ErrorCode, Description: apiResp.Description}
	}

	var file TGFile
	if err := json.Unmarshal(apiResp.Result, &file); err != nil {
		return nil, fmt.Errorf("unmarshal getFile result: %w", err)
	}

	return &file, nil
}

// DownloadFile downloads a file from Telegram's file storage using the path
// returned by GetFile. The caller must close the returned ReadCloser.
func (c *TelegramAPIClient) DownloadFile(ctx context.Context, filePath string) (io.ReadCloser, error) {
	url := fmt.Sprintf("%s/file/bot%s/%s", c.baseURL, c.botToken, filePath)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("create file download request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("file download request failed: %w", c.redactToken(err))
	}

	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("file download failed with status %d", resp.StatusCode)
	}

	return resp.Body, nil
}

// DeleteMessage deletes a message from a chat.
func (c *TelegramAPIClient) DeleteMessage(ctx context.Context, chatID int64, messageID int64) error {
	body := deleteMessageRequest{
		ChatID:    chatID,
		MessageID: messageID,
	}

	jsonBody, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal deleteMessage request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", c.methodURL("deleteMessage"), bytes.NewReader(jsonBody))
	if err != nil {
		return fmt.Errorf("create deleteMessage request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("deleteMessage request failed: %w", c.redactToken(err))
	}
	defer resp.Body.Close()

	var apiResp apiResponse
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return fmt.Errorf("decode deleteMessage response: %w", err)
	}

	if !apiResp.OK {
		apiErr := &APIError{Code: apiResp.ErrorCode, Description: apiResp.Description}
		if apiResp.Parameters != nil {
			apiErr.RetryAfterSec = apiResp.Parameters.RetryAfterSec
			apiErr.MigrateToChatID = apiResp.Parameters.MigrateToChatID
		}
		return apiErr
	}

	return nil
}
