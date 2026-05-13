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
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot"`
	FirstName string `json:"first_name"`
	Username  string `json:"username"`
}

// Update represents a Telegram update from getUpdates.
type Update struct {
	UpdateID      int64          `json:"update_id"`
	Message       *TGMessage     `json:"message,omitempty"`
	CallbackQuery *CallbackQuery `json:"callback_query,omitempty"`
}

// TGMessage represents a Telegram message.
type TGMessage struct {
	MessageID      int64           `json:"message_id"`
	From           *TGUser         `json:"from,omitempty"`
	Chat           TGChat          `json:"chat"`
	Date           int64           `json:"date"`
	Text           string          `json:"text"`
	Entities       []MessageEntity `json:"entities,omitempty"`
	ReplyToMessage *TGMessage      `json:"reply_to_message,omitempty"`
}

// MessageEntity represents a special entity in a Telegram message (e.g. @mentions, commands).
type MessageEntity struct {
	Type   string `json:"type"`
	Offset int    `json:"offset"`
	Length int    `json:"length"`
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
	RetryAfterSec int `json:"retry_after,omitempty"`
}

// APIError represents a non-OK response from the Telegram Bot API.
type APIError struct {
	Code          int
	Description   string
	RetryAfterSec int
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
		}
		return nil, apiErr
	}

	var msg TGMessage
	if err := json.Unmarshal(apiResp.Result, &msg); err != nil {
		return nil, fmt.Errorf("unmarshal sendMessage result: %w", err)
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
		}
		return apiErr
	}

	return nil
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
		}
		return apiErr
	}

	return nil
}
