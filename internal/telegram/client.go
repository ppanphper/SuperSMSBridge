package telegram

import (
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	tgbotapi "github.com/OvyFlash/telegram-bot-api"
)

type Client struct {
	bot     *tgbotapi.BotAPI
	groupID int64
	cache   *TopicCache
}

func NewClient(token string, groupID int64, dataDir string) (*Client, error) {
	// 直接构造 BotAPI,刻意跳过 NewBotAPI 在启动时的 getMe 网络调用。
	// 原因:当服务器到 api.telegram.org 的网络暂时不可达(典型表现为 EOF)时,
	// 原本的 getMe 会失败,main 里的 log.Fatalf 会让进程退出,docker 的
	// restart 策略又立即拉起,从而陷入崩溃循环,直到网络恢复才能启动。
	// 改为延迟连接:HTTP 服务照常起来,真正发送消息时再连 Telegram 并重试。
	bot := &tgbotapi.BotAPI{
		Token:  token,
		Client: &http.Client{Timeout: 30 * time.Second}, // 避免连接被对端无响应关闭时长时间挂起
		Buffer: 100,
	}
	bot.SetAPIEndpoint(tgbotapi.APIEndpoint)

	cache, err := NewTopicCache(dataDir)
	if err != nil {
		return nil, fmt.Errorf("初始化 Topic 缓存失败: %w", err)
	}

	// 后台探测一次连通性,仅用于启动日志提示,失败不影响服务启动。
	go func() {
		if me, err := bot.GetMe(); err != nil {
			log.Printf("警告: 启动时连接 Telegram 失败(将在发送消息时重试): %v", err)
		} else {
			log.Printf("已连接 Telegram Bot: @%s", me.UserName)
		}
	}()

	return &Client{
		bot:     bot,
		groupID: groupID,
		cache:   cache,
	}, nil
}

// isThreadNotFound 判断错误是否为"话题已失效"(话题在群里被删除)。
// 这类错误重试同一请求没有意义,需要清缓存重建话题。
func isThreadNotFound(err error) bool {
	return err != nil && strings.Contains(err.Error(), "message thread not found")
}

// sendWithRetry 发送请求并对瞬时网络错误(如 EOF)做有限重试。
func (c *Client) sendWithRetry(chattable tgbotapi.Chattable) (tgbotapi.Message, error) {
	const maxAttempts = 3
	var msg tgbotapi.Message
	var err error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		msg, err = c.bot.Send(chattable)
		if err == nil {
			return msg, nil
		}
		// 话题失效属于不可重试错误,立即返回交由上层重建话题。
		if isThreadNotFound(err) {
			return msg, err
		}
		log.Printf("Telegram 发送失败(第 %d/%d 次): %v", attempt, maxAttempts, err)
		if attempt < maxAttempts {
			time.Sleep(time.Duration(attempt) * time.Second)
		}
	}
	return msg, err
}

// getOrCreateTopic 获取或创建topic
func (c *Client) getOrCreateTopic(sender string) (int, error) {
	// 先从缓存中查找
	if topicID, exists := c.cache.GetTopicID(c.groupID, sender); exists {
		return topicID, nil
	}

	// 创建新topic
	createConfig := tgbotapi.CreateForumTopicConfig{
		ChatConfig: tgbotapi.ChatConfig{ChatID: c.groupID},
		Name:       sender,
	}

	msg, err := c.sendWithRetry(createConfig)
	if err != nil {
		return 0, fmt.Errorf("创建topic失败: %w", err)
	}

	// 保存到缓存
	topicID := msg.MessageThreadID
	if err := c.cache.SetTopicID(c.groupID, sender, topicID); err != nil {
		log.Printf("警告: 保存topic缓存失败: %v", err)
	}

	return topicID, nil
}

// SendMessage 发送消息到指定sender对应的topic
func (c *Client) SendMessage(sender, text string) error {
	topicID, err := c.getOrCreateTopic(sender)
	if err != nil {
		return err
	}

	err = c.sendToTopic(topicID, text)
	if err == nil {
		return nil
	}

	// 话题可能已在群里被删除,缓存的 thread 失效。清除缓存重建话题后重试一次。
	if isThreadNotFound(err) {
		log.Printf("话题已失效(sender=%q, topicID=%d),清除缓存并重建话题后重试", sender, topicID)
		if derr := c.cache.DeleteTopicID(c.groupID, sender); derr != nil {
			log.Printf("警告: 清除话题缓存失败: %v", derr)
		}
		newTopicID, cerr := c.getOrCreateTopic(sender)
		if cerr != nil {
			return cerr
		}
		return c.sendToTopic(newTopicID, text)
	}

	return err
}

// sendToTopic 把文本发送到指定话题。
func (c *Client) sendToTopic(topicID int, text string) error {
	msg := tgbotapi.NewMessage(c.groupID, text)
	msg.MessageThreadID = topicID

	if _, err := c.sendWithRetry(msg); err != nil {
		return fmt.Errorf("发送消息失败: %w", err)
	}
	return nil
}
