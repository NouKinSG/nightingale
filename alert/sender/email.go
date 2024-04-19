package sender

import (
	"crypto/tls"
	"errors"
	"html/template"
	"time"

	"github.com/ccfos/nightingale/v6/alert/aconf"
	"github.com/ccfos/nightingale/v6/memsto"
	"github.com/ccfos/nightingale/v6/models"

	"github.com/toolkits/pkg/logger"

	"gopkg.in/gomail.v2"
)

// 这是一个 邮箱通道
var mailch chan *gomail.Message

// 邮箱发送器
type EmailSender struct {
	subjectTpl *template.Template
	contentTpl *template.Template
	smtp       aconf.SMTPConfig
}

// 处理告警事件的时候会去发送邮件
func (es *EmailSender) Send(ctx MessageContext) {
	if len(ctx.Users) == 0 || len(ctx.Events) == 0 {
		return
	}
	tos := extract(ctx.Users)
	var subject string

	if es.subjectTpl != nil {
		subject = BuildTplMessage(models.Email, es.subjectTpl, []*models.AlertCurEvent{ctx.Events[0]})
	} else {
		subject = ctx.Events[0].RuleName
	}
	content := BuildTplMessage(models.Email, es.contentTpl, ctx.Events)
	es.WriteEmail(subject, content, tos)

	ctx.Stats.AlertNotifyTotal.WithLabelValues(models.Email).Add(float64(len(tos)))
}

// extract 提取 用户对象的邮箱地址
func extract(users []*models.User) []string {
	tos := make([]string, 0, len(users))
	for _, u := range users {
		if u.Email != "" {
			tos = append(tos, u.Email)
		}
	}
	return tos
}

// 测试连接时，会去 模拟发送一次
func SendEmail(subject, content string, tos []string, stmp aconf.SMTPConfig) error {
	conf := stmp

	d := gomail.NewDialer(conf.Host, conf.Port, conf.User, conf.Pass)
	if conf.InsecureSkipVerify {
		d.TLSConfig = &tls.Config{InsecureSkipVerify: true}
	}

	m := gomail.NewMessage()

	m.SetHeader("From", stmp.From)
	m.SetHeader("To", tos...)
	m.SetHeader("Subject", subject)
	m.SetBody("text/html", content)

	err := d.DialAndSend(m)
	if err != nil {
		return errors.New("email_sender: failed to send: " + err.Error())
	}
	return nil
}

// 邮件的内容
func (es *EmailSender) WriteEmail(subject, content string, tos []string) {
	m := gomail.NewMessage()

	m.SetHeader("From", es.smtp.From)
	m.SetHeader("To", tos...)
	m.SetHeader("Subject", subject)
	m.SetBody("text/html", content)

	mailch <- m
}

// 拨号 SMTP
func dialSmtp(d *gomail.Dialer) gomail.SendCloser {
	for {
		if s, err := d.Dial(); err != nil {
			logger.Errorf("email_sender: failed to dial smtp: %s", err)
			time.Sleep(time.Second)
			continue
		} else {
			return s
		}
	}
}

var mailQuit = make(chan struct{})

// 重置邮件发送器
func RestartEmailSender(smtp aconf.SMTPConfig) {
	close(mailQuit)
	mailQuit = make(chan struct{})
	startEmailSender(smtp)
}

var smtpConfig aconf.SMTPConfig

// 初始化时，start下 go InitEmailSender
func InitEmailSender(ncc *memsto.NotifyConfigCacheType) {
	mailch = make(chan *gomail.Message, 100000)
	go updateSmtp(ncc)
	smtpConfig = ncc.GetSMTP()
	startEmailSender(smtpConfig)
}

// 更新 SMTP ，在 go InitEmailSender 下
func updateSmtp(ncc *memsto.NotifyConfigCacheType) {
	// 每隔一段时间去配置 SMTP
	for {
		time.Sleep(1 * time.Minute)
		smtp := ncc.GetSMTP()
		if smtpConfig.Host != smtp.Host || smtpConfig.Batch != smtp.Batch || smtpConfig.From != smtp.From ||
			smtpConfig.Pass != smtp.Pass || smtpConfig.User != smtp.User || smtpConfig.Port != smtp.Port ||
			smtpConfig.InsecureSkipVerify != smtp.InsecureSkipVerify { //diff
			smtpConfig = smtp
			RestartEmailSender(smtp)
		}
	}
}

// 真正开始邮件发送器的配置
func startEmailSender(smtp aconf.SMTPConfig) {
	conf := smtp
	if conf.Host == "" || conf.Port == 0 {
		logger.Warning("SMTP configurations invalid")
		return
	}
	logger.Infof("start email sender... conf.Host:%+v,conf.Port:%+v", conf.Host, conf.Port)

	d := gomail.NewDialer(conf.Host, conf.Port, conf.User, conf.Pass)
	if conf.InsecureSkipVerify {
		d.TLSConfig = &tls.Config{InsecureSkipVerify: true}
	}

	var s gomail.SendCloser
	var open bool
	var size int
	for {
		select {
		case <-mailQuit:
			return
		case m, ok := <-mailch:
			if !ok {
				return
			}

			if !open {
				s = dialSmtp(d)
				open = true
			}
			if err := gomail.Send(s, m); err != nil {
				logger.Errorf("email_sender: failed to send: %s", err)

				// close and retry
				if err := s.Close(); err != nil {
					logger.Warningf("email_sender: failed to close smtp connection: %s", err)
				}

				s = dialSmtp(d)
				open = true

				if err := gomail.Send(s, m); err != nil {
					logger.Errorf("email_sender: failed to retry send: %s", err)
				}
			} else {
				logger.Infof("email_sender: result=succ subject=%v to=%v", m.GetHeader("Subject"), m.GetHeader("To"))
			}

			size++

			if size >= conf.Batch {
				if err := s.Close(); err != nil {
					logger.Warningf("email_sender: failed to close smtp connection: %s", err)
				}
				open = false
				size = 0
			}

		// Close the connection to the SMTP server if no email was sent in
		// the last 30 seconds.
		//如果没有发送电子邮件，请关闭与SMTP服务器的连接
		//最后 30 秒。
		case <-time.After(30 * time.Second):
			if open {
				if err := s.Close(); err != nil {
					logger.Warningf("email_sender: failed to close smtp connection: %s", err)
				}
				open = false
			}
		}
	}
}
