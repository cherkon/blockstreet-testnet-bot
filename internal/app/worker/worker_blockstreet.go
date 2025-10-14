package worker

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/widiskel/blockstreet-testnet-bot/internal/adapters/captcha"
	"github.com/widiskel/blockstreet-testnet-bot/internal/adapters/chain"
	adhttp "github.com/widiskel/blockstreet-testnet-bot/internal/adapters/http"
	"github.com/widiskel/blockstreet-testnet-bot/internal/config"
	"github.com/widiskel/blockstreet-testnet-bot/internal/domain/model"
	"github.com/widiskel/blockstreet-testnet-bot/internal/platform/logger"
	"github.com/widiskel/blockstreet-testnet-bot/internal/storage/signlog"
)

const (
	blockStreetDomain           = "blockstreet.money"
	blockStreetURI              = "https://blockstreet.money"
	blockStreetAPI              = "https://api.blockstreet.money"
	blockStreetStatement        = "Welcome to Block Street"
	creatorInviteCode           = "K6SP6a"
	blockStreetTurnstileSiteKey = "0x4AAAAAABpfyUqunlqwRBYN"
	blockStreetTurnstilePageURL = "https://blockstreet.money"
	defaultTokenTTL             = 90 * time.Second
	capSolverMaxAttempts        = 3
	twoCaptchaMaxAttempts       = 3
	solverInitialDelay          = 5 * time.Second
	signVerifyMaxAttempts       = 3
)

var defaultHeaders = map[string]string{
	"Origin":  blockStreetURI,
	"Referer": blockStreetURI + "/",
}

var (
	capSolverDisabled  atomic.Bool
	twoCaptchaDisabled atomic.Bool
	errNoCaptchaCredit = errors.New("captcha solver credit exhausted")
)

type TurnstileCache struct {
	mu        sync.Mutex
	token     string
	expiresAt time.Time
}

func (c *TurnstileCache) Get() (string, bool) {
	if c == nil {
		return "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token == "" || time.Now().After(c.expiresAt) {
		return "", false
	}
	return c.token, true
}

func (c *TurnstileCache) Set(token string, ttl time.Duration) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.token = token
	c.expiresAt = time.Now().Add(ttl)
	c.mu.Unlock()
}

func (c *TurnstileCache) Invalidate() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.token = ""
	c.expiresAt = time.Time{}
	c.mu.Unlock()
}

type BlockStreetWorker struct {
	ec              *chain.EthersClient
	apiClient       *adhttp.APIClient
	log             *logger.ClassLogger
	cfg             config.Config
	account         config.Account
	network         config.Network
	store           *signlog.Store
	session         *model.Session
	inviteCode      string
	isChild         bool
	cache           *TurnstileCache
	useCaptchaCache bool
	sessionValid    bool
}

type signNonceResponse struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    struct {
		SignNonce string `json:"signnonce"`
	} `json:"data"`
}

type signVerifyPayload struct {
	Address        string `json:"address"`
	Nonce          string `json:"nonce"`
	Signature      string `json:"signature"`
	ChainID        int    `json:"chainId"`
	IssuedAt       string `json:"issuedAt"`
	ExpirationTime string `json:"expirationTime"`
	InviteCode     string `json:"invite_code,omitempty"`
}

type signVerifyResponse struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    struct {
		Result bool `json:"rst"`
	} `json:"data"`
}

func executeBlockStreetOperation(ec *chain.EthersClient, apiClient *adhttp.APIClient, log *logger.ClassLogger, cfg config.Config, account config.Account, store *signlog.Store, session *model.Session, inviteCode string, cache *TurnstileCache, isChild bool) error {
	if cache == nil {
		cache = &TurnstileCache{}
	}
	worker := &BlockStreetWorker{
		ec:              ec,
		apiClient:       apiClient,
		log:             log,
		cfg:             cfg,
		account:         account,
		network:         config.MonadTestnet,
		store:           store,
		session:         session,
		inviteCode:      inviteCode,
		isChild:         isChild,
		cache:           cache,
		useCaptchaCache: !isChild,
		sessionValid:    false,
	}
	return worker.Operate()
}

func (w *BlockStreetWorker) Operate() error {
	address := w.ec.Address()
	if address == "" {
		return fmt.Errorf("wallet address unavailable for signing")
	}
	if w.session != nil {
		w.session.Address = address
	}

	today := time.Now().UTC()

	if w.isChild {
		return w.performLogin(w.inviteCode, today, false)
	}

	var (
		loginDone        bool
		inviteDone       bool
		shareDone        bool
		target           int
		completed        int
		storedMin        int
		storedMax        int
		todayEarn        string
		totalEarn        string
		balance          string
		storedInviteDone bool
	)
	if w.store != nil {
		var err error
		loginDone, inviteDone, shareDone, target, completed, storedMin, storedMax, todayEarn, totalEarn, balance, err = w.store.DailyStatus(address, today)
		if err != nil {
			w.log.Log(fmt.Sprintf("Warning: failed checking sign log: %v", err))
		}
		storedInviteDone = inviteDone
	}

	if w.session != nil && !w.isChild {
		if loginDone {
			w.setLoginStatus(statusDone)
		} else if strings.TrimSpace(w.session.DailyLoginStatus) == "" {
			w.setLoginStatus(statusWaiting)
		}
	}

	minBound, maxBound := normalizeInviteBounds(w.cfg.InviteMin, w.cfg.InviteMax)
	shouldRefreshTarget := false
	if target <= 0 && (minBound > 0 || maxBound > 0) {
		shouldRefreshTarget = true
	}
	if storedMin != minBound || storedMax != maxBound {
		shouldRefreshTarget = true
	}

	if shouldRefreshTarget {
		if minBound == 0 && maxBound == 0 {
			target = 0
		} else {
			target = w.generateInviteTarget(minBound, maxBound)
		}
		if w.store != nil {
			if err := w.store.SetInviteTarget(address, today, target, minBound, maxBound); err != nil {
				w.log.Log(fmt.Sprintf("Warning: failed to set invite target: %v", err))
			} else {
				storedMin = minBound
				storedMax = maxBound
			}
		} else {
			storedMin = minBound
			storedMax = maxBound
		}
		w.log.Log(fmt.Sprintf("Invite target refreshed (min=%d max=%d target=%d)", storedMin, storedMax, target))
	}

	inviteDone = target <= 0 || completed >= target

	sessionInfo, loginStatus, err := w.ensureSession(loginDone, today)
	if err != nil {
		return err
	}
	if !w.isChild {
		if loginStatus {
			w.setLoginStatus(statusDone)
		} else {
			w.setLoginStatus(statusWaiting)
		}
	}

	if w.session != nil {
		if storedInviteDone {
			w.setInviteStatus(statusDone)
		} else if w.session.DailyInviteStatus == statusDone {
			w.setInviteStatus(statusWaiting)
		}
		w.session.InviteTarget = target
		w.session.InviteCompleted = completed
		if todayEarn != "" {
			w.session.TodayEarn = todayEarn
		}
		if totalEarn != "" {
			w.session.TotalEarn = totalEarn
		}
		if balance != "" {
			w.session.BalanceEarn = balance
		}
		if shareDone {
			w.setShareStatus(statusDone)
		} else if strings.TrimSpace(w.session.DailyShareStatus) == "" {
			w.setShareStatus(statusWaiting)
		}
	}
	if shareDone {
		if err := w.fetchEarnings(today); err != nil {
			w.log.Log(fmt.Sprintf("Warning: failed to refresh earning info: %v", err))
		}
	} else {
		if err := w.handleEarningsAndShare(today, false); err != nil {
			return err
		}
		shareDone = true
	}

	if sessionInfo == nil {
		return fmt.Errorf("received empty user info after session validation")
	}

	inviteCode := strings.TrimSpace(sessionInfo.InviteCode)
	if inviteCode == "" {
		return fmt.Errorf("received empty invite code")
	}

	if inviteDone {
		w.setInviteStatus(statusDone)
		w.log.Log("BlockStreet daily tasks completed")
		return nil
	}

	if target <= 0 {
		w.setInviteStatus(statusDone)
		w.log.Log("BlockStreet daily tasks completed")
		return nil
	}

	if completed >= target {
		w.setInviteStatus(statusDone)
		w.log.Log("BlockStreet daily tasks completed")
		return nil
	}

	w.setInviteStatus(statusInProgress)
	invitesNeeded := target - completed
	for i := 0; i < invitesNeeded; i++ {
		if err := w.performInvite(inviteCode); err != nil {
			return err
		}
		if w.store != nil {
			newCompleted, err := w.store.IncrementInvite(address, today)
			if err != nil {
				w.log.Log(fmt.Sprintf("Warning: failed to update invite progress: %v", err))
			} else {
				completed = newCompleted
			}
		} else {
			completed++
		}
		if w.session != nil {
			w.session.InviteCompleted = completed
			w.session.InviteTarget = target
		}
		if i < invitesNeeded-1 {
			if delay := randomInviteDelay(w.cfg.InviteDelayMin, w.cfg.InviteDelayMax); delay > 0 {
				w.log.Log(fmt.Sprintf("Waiting %s before next invite", delay), int(delay/time.Millisecond))
				time.Sleep(delay)
			}
		}
	}

	w.setInviteStatus(statusDone)
	w.log.Log("BlockStreet daily tasks completed")
	return nil
}

func (w *BlockStreetWorker) signNonce() (string, error) {
	headers := cloneDefaultHeaders()
	w.log.Log("Requesting sign nonce from BlockStreet")
	rawNonceRes, err := w.apiClient.Fetch(blockStreetAPI+"/api/account/signnonce", &adhttp.FetchOptions{
		Method:            "GET",
		AdditionalHeaders: headers,
	})
	if err != nil {
		return "", fmt.Errorf("failed to fetch signnonce: %w", err)
	}

	var nonceRes signNonceResponse
	if err := decodeInto(rawNonceRes, &nonceRes); err != nil {
		return "", fmt.Errorf("failed to decode signnonce response: %w", err)
	}

	if nonceRes.Code != 0 {
		return "", fmt.Errorf("signnonce API error: code=%d message=%s", nonceRes.Code, nonceRes.Message)
	}

	nonce := strings.TrimSpace(nonceRes.Data.SignNonce)
	if nonce == "" {
		return "", fmt.Errorf("received empty sign nonce")
	}

	w.log.Log(fmt.Sprintf("Received sign nonce: %s", nonce))
	return nonce, nil
}

func (w *BlockStreetWorker) signVerify(payload signVerifyPayload) error {
	if !w.isChild && w.store != nil && w.session != nil && w.sessionValid {
		address := strings.TrimSpace(w.session.Address)
		if address != "" {
			loginDone, _, _, _, _, _, _, _, _, _, err := w.store.DailyStatus(address, time.Now().UTC())
			if err == nil && loginDone {
				w.log.Log("Daily login already verified and session valid; skipping signVerify")
				return nil
			}
		}
	}

	var lastErr error
	for attempt := 1; attempt <= signVerifyMaxAttempts; attempt++ {
		verifyHeaders := cloneDefaultHeaders()
		w.log.Log("Solving Cloudflare Turnstile captcha for signverify request")
		token, err := w.fetchTurnstileToken()
		if err != nil {
			lastErr = fmt.Errorf("failed to solve turnstile: %w", err)
			break
		}
		if token != "" {
			verifyHeaders["cf-turnstile-response"] = token
		}

		w.log.Log("Submitting signature to BlockStreet signverify endpoint")
		rawVerifyRes, err := w.apiClient.Fetch(blockStreetAPI+"/api/account/signverify", &adhttp.FetchOptions{
			Method:            "POST",
			Body:              payload,
			AdditionalHeaders: verifyHeaders,
		})
		if err != nil {
			if w.shouldUseCaptchaCache() {
				w.cache.Invalidate()
			}
			lastErr = fmt.Errorf("signverify request failed: %w", err)
			break
		}

		var verifyRes signVerifyResponse
		if err := decodeInto(rawVerifyRes, &verifyRes); err != nil {
			if w.shouldUseCaptchaCache() {
				w.cache.Invalidate()
			}
			lastErr = fmt.Errorf("failed to decode signverify response: %w", err)
			break
		}

		if verifyRes.Code == 0 && verifyRes.Data.Result {
			w.log.Log("BlockStreet signverify succeeded")
			return nil
		}

		if w.shouldUseCaptchaCache() {
			w.cache.Invalidate()
		}
		lastErr = fmt.Errorf("signverify rejected: code=%d message=%s", verifyRes.Code, verifyRes.Message)
		if attempt < signVerifyMaxAttempts && w.shouldRetrySignVerify(verifyRes.Code, verifyRes.Message) {
			w.log.Log(fmt.Sprintf("signverify attempt %d failed with code %d (%s), retrying with fresh Turnstile token", attempt, verifyRes.Code, verifyRes.Message))
			continue
		}

		break
	}

	return lastErr
}

func (w *BlockStreetWorker) ensureSession(loginDone bool, day time.Time) (*userInfoData, bool, error) {
	if w.session == nil {
		return nil, loginDone, nil
	}

	w.sessionValid = false

	if w.isChild {
		if err := w.performLogin(w.inviteCode, day, !loginDone); err != nil {
			return nil, loginDone, err
		}
		loginDone = true
		info, err := w.getUserInfo()
		if err != nil {
			return nil, loginDone, err
		}
		if info != nil && strings.TrimSpace(info.WalletAddress) != "" {
			w.sessionValid = true
		}
		return info, loginDone, nil
	}

	requiresLogin := !loginDone || !w.apiClient.HasCookies()
	if requiresLogin {
		if !loginDone {
			w.log.Log("Daily login not recorded, performing BlockStreet signVerify")
		} else {
			w.log.Log("Session cookies missing, performing BlockStreet signVerify")
		}
		if err := w.performLogin(creatorInviteCode, day, !loginDone); err != nil {
			return nil, loginDone, err
		}
		loginDone = true
	}

	info, err := w.getUserInfo()
	if err == nil && info != nil && strings.TrimSpace(info.WalletAddress) != "" {
		w.sessionValid = true
		return info, loginDone, nil
	}

	w.log.Log("Session cookies appear invalid, refreshing BlockStreet authentication")
	if err := w.apiClient.ClearCookies(); err != nil {
		w.log.Log(fmt.Sprintf("Warning: failed to clear session cookies: %v", err))
	}
	if err := w.performLogin(creatorInviteCode, day, false); err != nil {
		return nil, loginDone, err
	}
	loginDone = true
	info, err = w.getUserInfo()
	if err != nil {
		return nil, loginDone, fmt.Errorf("failed to fetch user info after refreshing session: %w", err)
	}
	if info == nil || strings.TrimSpace(info.WalletAddress) == "" {
		return nil, loginDone, errors.New("received empty user info after refreshing session")
	}
	w.sessionValid = true
	return info, loginDone, nil
}

func (w *BlockStreetWorker) performLogin(inviteCode string, day time.Time, markStore bool) error {
	if inviteCode == "" {
		inviteCode = creatorInviteCode
	}
	if !w.isChild {
		w.setLoginStatus(statusInProgress)
	}
	if err := w.executeSignFlow(inviteCode); err != nil {
		if !w.isChild {
			w.setLoginStatus(statusWaiting)
		}
		return err
	}
	if markStore && w.store != nil && w.session != nil {
		if err := w.store.MarkLogin(w.session.Address, day); err != nil {
			w.log.Log(fmt.Sprintf("Warning: failed to mark login log: %v", err))
		}
	}
	if !w.isChild {
		w.setLoginStatus(statusDone)
	}
	return nil
}

func (w *BlockStreetWorker) executeSignFlow(inviteCode string) error {
	nonce, err := w.signNonce()
	if err != nil {
		return fmt.Errorf("blockstreet signNonce failed: %w", err)
	}

	address := w.ec.Address()
	if address == "" {
		return fmt.Errorf("wallet address unavailable for signing")
	}

	issuedAt := time.Now().UTC()
	expiration := issuedAt.Add(2 * time.Minute)
	chainID := w.network.ChainID
	message := fmt.Sprintf(`%s wants you to sign in with your Ethereum account:
%s

%s

URI: %s
Version: 1
Chain ID: %d
Nonce: %s
Issued At: %s
Expiration Time: %s`,
		blockStreetDomain,
		address,
		blockStreetStatement,
		blockStreetURI,
		chainID,
		nonce,
		formatTimeISO8601Z(issuedAt),
		formatTimeISO8601Z(expiration),
	)

	w.log.Log(fmt.Sprintf("Preparing BlockStreet SIWE payload (address=%s nonce=%s invite=%s chainId=%d)", address, nonce, strings.TrimSpace(inviteCode), chainID))
	signature, err := w.ec.SignMessage(message)
	if err != nil {
		return fmt.Errorf("failed to sign SIWE message: %w", err)
	}

	w.log.Log(fmt.Sprintf("SIWE payload ready (issuedAt=%s expiresAt=%s signature=%s...); solving Turnstile captcha next", formatTimeISO8601Z(issuedAt), formatTimeISO8601Z(expiration), truncateForLog(signature, 16)))

	payload := buildSignVerifyPayload(address, nonce, signature, inviteCode, chainID, issuedAt, expiration)
	if err := w.signVerify(payload); err != nil {
		return fmt.Errorf("blockstreet signVerify failed: %w", err)
	}
	return nil
}

func (w *BlockStreetWorker) performInvite(inviteCode string) error {
	if inviteCode == "" {
		return fmt.Errorf("empty invite code")
	}

	childPK, err := chain.GeneratePrivateKeyHex()
	if err != nil {
		return fmt.Errorf("failed to generate child wallet: %w", err)
	}
	childAccount := config.Account{PrivateKey: childPK}
	childSession := model.Session{Account: childPK, AccIdx: w.session.AccIdx, Address: "-", Role: "child", Parent: w.session}
	childSession.DailyLoginStatus = statusWaiting
	childSession.DailyInviteStatus = statusWaiting
	childSession.DailyShareStatus = statusWaiting
	childSession.InviteTarget = 0
	childSession.InviteCompleted = 0
	childSession.TodayEarn = "0"
	childSession.TotalEarn = "0"
	childSession.BalanceEarn = "0"

	w.log.Log("Generating child invite wallet for BlockStreet daily target")

	cookieFile := cookieFilePath(childSession.AccIdx, childAccount)
	defer func() {
		if err := os.Remove(cookieFile); err != nil && !errors.Is(err, os.ErrNotExist) {
			w.log.Log(fmt.Sprintf("Warning: failed to cleanup cookie file %s: %v", cookieFile, err))
		}
	}()
	childAPI, err := adhttp.NewAPIClient("", cookieFile, &childSession)
	if err != nil {
		return fmt.Errorf("failed to init child api client: %w", err)
	}

	childEC := w.ec.CloneForSession(&childSession)
	if err := childEC.ConnectWallet(); err != nil {
		return fmt.Errorf("failed to connect child wallet: %w", err)
	}

	if err := executeBlockStreetOperation(childEC, childAPI, w.log, w.cfg, childAccount, nil, &childSession, inviteCode, w.cache, true); err != nil {
		w.setInviteStatus(statusWaiting)
		return fmt.Errorf("child invite flow failed: %w", err)
	}

	return nil
}

type userInfoResponse struct {
	Code    int          `json:"code"`
	Message string       `json:"message"`
	Data    userInfoData `json:"data"`
}

type userInfoData struct {
	WalletAddress string `json:"wallet_address"`
	InviteCode    string `json:"invite_code"`
}

func (w *BlockStreetWorker) getUserInfo() (*userInfoData, error) {
	raw, err := w.apiClient.Fetch(blockStreetAPI+"/api/account/info", &adhttp.FetchOptions{Method: "GET"})
	if err != nil {
		return nil, err
	}
	var resp userInfoResponse
	if err := decodeInto(raw, &resp); err != nil {
		return nil, err
	}
	if resp.Code != 0 {
		return nil, fmt.Errorf("user info API error: code=%d message=%s", resp.Code, resp.Message)
	}
	return &resp.Data, nil
}

func (w *BlockStreetWorker) setLoginStatus(status string) {
	if w.session != nil {
		w.session.DailyLoginStatus = status
	}
}

func (w *BlockStreetWorker) setInviteStatus(status string) {
	if w.session != nil {
		w.session.DailyInviteStatus = status
	}
}

func (w *BlockStreetWorker) setShareStatus(status string) {
	if w.session != nil {
		w.session.DailyShareStatus = status
	}
}

func normalizeInviteBounds(min, max int) (int, int) {
	if min < 0 {
		min = 0
	}
	if max < 0 {
		max = 0
	}
	if max < min {
		max = min
	}
	return min, max
}

func (w *BlockStreetWorker) generateInviteTarget(min, max int) int {
	min, max = normalizeInviteBounds(min, max)
	if min == 0 && max == 0 {
		return 0
	}
	if min == max {
		return min
	}
	delta := max - min + 1
	val, err := rand.Int(rand.Reader, big.NewInt(int64(delta)))
	if err != nil {
		return min
	}
	return min + int(val.Int64())
}

func randomInviteDelay(minMinutes, maxMinutes int) time.Duration {
	if minMinutes <= 0 && maxMinutes <= 0 {
		return 0
	}
	if minMinutes < 0 {
		minMinutes = 0
	}
	if maxMinutes < minMinutes {
		maxMinutes = minMinutes
	}
	if minMinutes == maxMinutes {
		return time.Duration(minMinutes) * time.Minute
	}
	delta := maxMinutes - minMinutes + 1
	val, err := rand.Int(rand.Reader, big.NewInt(int64(delta)))
	if err != nil {
		return time.Duration(minMinutes) * time.Minute
	}
	totalMinutes := minMinutes + int(val.Int64())
	return time.Duration(totalMinutes) * time.Minute
}

func (w *BlockStreetWorker) handleEarningsAndShare(day time.Time, shareAlreadyDone bool) error {
	if err := w.fetchEarnings(day); err != nil {
		w.log.Log(fmt.Sprintf("Warning: failed to fetch earning info: %v", err))
	}

	if shareAlreadyDone {
		w.setShareStatus(statusDone)
		return nil
	}

	return w.performShare(day)
}

type earnInfoResponse struct {
	Code    int          `json:"code"`
	Message string       `json:"message"`
	Data    earnInfoData `json:"data"`
}

type earnInfoData struct {
	TodayEarn string `json:"today_earn"`
	TotalEarn string `json:"total_earn"`
	Balance   string `json:"balance"`
}

func (w *BlockStreetWorker) fetchEarnings(day time.Time) error {
	raw, err := w.apiClient.Fetch(blockStreetAPI+"/api/earn/info", &adhttp.FetchOptions{Method: "GET"})
	if err != nil {
		return err
	}
	var resp earnInfoResponse
	if err := decodeInto(raw, &resp); err != nil {
		return err
	}
	if resp.Code != 0 {
		return fmt.Errorf("earn info API error: code=%d message=%s", resp.Code, resp.Message)
	}

	if w.session != nil {
		w.session.TodayEarn = resp.Data.TodayEarn
		w.session.TotalEarn = resp.Data.TotalEarn
		w.session.BalanceEarn = resp.Data.Balance
	}

	if w.store != nil && w.session != nil {
		if err := w.store.UpdateEarning(w.session.Address, day, resp.Data.TodayEarn, resp.Data.TotalEarn, resp.Data.Balance); err != nil {
			w.log.Log(fmt.Sprintf("Warning: failed to update earning log: %v", err))
		}
	}

	return nil
}

type shareResponse struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    bool   `json:"data"`
}

func (w *BlockStreetWorker) performShare(day time.Time) error {
	if w.session != nil {
		w.setShareStatus(statusInProgress)
	}

	body := map[string]string{}
	raw, err := w.apiClient.Fetch(blockStreetAPI+"/api/share", &adhttp.FetchOptions{
		Method: "POST",
		Body:   body,
	})
	if err != nil {
		w.setShareStatus(statusWaiting)
		return fmt.Errorf("share request failed: %w", err)
	}

	var resp shareResponse
	if err := decodeInto(raw, &resp); err != nil {
		w.setShareStatus(statusWaiting)
		return err
	}
	if resp.Code != 0 {
		w.setShareStatus(statusWaiting)
		return fmt.Errorf("share API error: code=%d message=%s", resp.Code, resp.Message)
	}

	if w.store != nil && w.session != nil {
		if err := w.store.MarkShare(w.session.Address, day); err != nil {
			w.log.Log(fmt.Sprintf("Warning: failed to mark share log: %v", err))
		}
	}
	if w.session != nil {
		w.setShareStatus(statusDone)
	}
	return nil
}

func cloneDefaultHeaders() map[string]string {
	headers := make(map[string]string, len(defaultHeaders))
	for k, v := range defaultHeaders {
		headers[k] = v
	}
	return headers
}

func decodeInto(in interface{}, out interface{}) error {
	bytes, err := json.Marshal(in)
	if err != nil {
		return err
	}
	return json.Unmarshal(bytes, out)
}

func truncateForLog(value string, length int) string {
	if length <= 0 || len(value) <= length {
		return value
	}
	return value[:length]
}

func formatTimeISO8601Z(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z07:00")
}

func buildSignVerifyPayload(address, nonce, signature, inviteCode string, chainID int, issuedAt, expiration time.Time) signVerifyPayload {
	return signVerifyPayload{
		Address:        address,
		Nonce:          nonce,
		Signature:      signature,
		ChainID:        chainID,
		IssuedAt:       formatTimeISO8601Z(issuedAt),
		ExpirationTime: formatTimeISO8601Z(expiration),
		InviteCode:     strings.TrimSpace(inviteCode),
	}
}

func (w *BlockStreetWorker) shouldRetrySignVerify(code int, message string) bool {
	if code == 5017 {
		return true
	}
	if strings.EqualFold(strings.TrimSpace(message), "verify failed") {
		return true
	}
	return false
}

func (w *BlockStreetWorker) shouldUseCaptchaCache() bool {
	return w.useCaptchaCache && w.cache != nil
}

func (w *BlockStreetWorker) fetchTurnstileToken() (string, error) {
	useCache := w.shouldUseCaptchaCache()
	if useCache {
		if token, ok := w.cache.Get(); ok {
			return token, nil
		}
	}

	capKey := strings.TrimSpace(w.cfg.CapSolverAPIKey)
	if capKey != "" && !capSolverDisabled.Load() {
		token, err := w.solveWithCapSolver(capKey, blockStreetTurnstileSiteKey, blockStreetTurnstilePageURL)
		if err != nil {
			if errors.Is(err, captcha.ErrZeroBalance) {
				w.log.Log("CapSolver reports zero balance, disabling CapSolver usage")
				capSolverDisabled.Store(true)
			} else {
				w.log.Log(fmt.Sprintf("CapSolver attempts exhausted (%v), falling back to 2Captcha", err))
			}
		} else if strings.TrimSpace(token) != "" {
			if useCache {
				w.cache.Set(token, defaultTokenTTL)
			}
			return token, nil
		}
	}

	twoKey := strings.TrimSpace(w.cfg.TwoCaptchaAPIKey)
	if twoKey != "" && !twoCaptchaDisabled.Load() {
		token, err := w.solveWithTwoCaptcha(twoKey, blockStreetTurnstileSiteKey, blockStreetTurnstilePageURL)
		if err != nil {
			if errors.Is(err, captcha.ErrZeroBalance) {
				w.log.Log("2Captcha reports zero balance, disabling 2Captcha usage")
				twoCaptchaDisabled.Store(true)
			} else {
				return "", err
			}
		} else if strings.TrimSpace(token) != "" {
			if useCache {
				w.cache.Set(token, defaultTokenTTL)
				w.log.Log("Received Turnstile token")
			} else {
				w.log.Log("Received Turnstile token (cache disabled)")
			}
			return token, nil
		}
	}

	capUnavailable := capSolverDisabled.Load() || capKey == ""
	twoUnavailable := twoCaptchaDisabled.Load() || twoKey == ""

	if capUnavailable && twoUnavailable {
		return "", errNoCaptchaCredit
	}

	return "", errors.New("unable to fetch captcha token")
}

func (w *BlockStreetWorker) solveWithCapSolver(apiKey, siteKey, pageURL string) (string, error) {
	solver := captcha.NewCapSolver(apiKey)
	delay := solverInitialDelay
	var lastErr error
	for attempt := 1; attempt <= capSolverMaxAttempts; attempt++ {
		token, err := solver.SolveTurnstile(siteKey, pageURL)
		if err == nil && strings.TrimSpace(token) != "" {
			w.log.Log(fmt.Sprintf("Received Turnstile token from CapSolver (attempt %d)", attempt))
			return token, nil
		}
		if errors.Is(err, captcha.ErrZeroBalance) {
			return "", captcha.ErrZeroBalance
		}
		lastErr = err
		if err != nil {
			w.log.Log(fmt.Sprintf("CapSolver attempt %d failed: %v", attempt, err))
		}
		if attempt < capSolverMaxAttempts {
			time.Sleep(delay)
			delay *= 2
		}
	}
	if lastErr != nil {
		return "", lastErr
	}
	return "", errors.New("capsolver attempts exhausted")
}

func (w *BlockStreetWorker) solveWithTwoCaptcha(apiKey, siteKey, pageURL string) (string, error) {
	solver := captcha.NewTwoCaptcha(apiKey)
	delay := solverInitialDelay
	var lastErr error
	for attempt := 1; attempt <= twoCaptchaMaxAttempts; attempt++ {
		token, err := solver.SolveTurnstile(siteKey, pageURL)
		if err == nil && strings.TrimSpace(token) != "" {
			w.log.Log(fmt.Sprintf("Received Turnstile token from 2Captcha (attempt %d)", attempt))
			return token, nil
		}
		if errors.Is(err, captcha.ErrZeroBalance) {
			return "", captcha.ErrZeroBalance
		}
		lastErr = err
		if err != nil {
			w.log.Log(fmt.Sprintf("2Captcha attempt %d failed: %v", attempt, err))
		}
		if attempt < twoCaptchaMaxAttempts {
			time.Sleep(delay)
			delay *= 2
		}
	}
	if lastErr != nil {
		return "", lastErr
	}
	return "", errors.New("2captcha attempts exhausted")
}
