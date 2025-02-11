package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/d--j/go-milter/integration"
	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"
)

var ErrTestSkipped = errors.New("test skipped")

type TestDir struct {
	Index      int
	Path       string
	Config     *Config
	MTA        *MTA
	Tests      []*TestCase
	cmd        *exec.Cmd
	wg         sync.WaitGroup
	once       sync.Once
	m          sync.Mutex
	startErr   error
	failedTest bool
}

func (t *TestDir) Start() error {
	p := path.Join(t.Config.ScratchDir, fmt.Sprintf("test-%d", t.Index))
	err := os.Mkdir(p, 0700)
	if err != nil && !os.IsExist(err) {
		return err
	}
	exe := path.Join(p, "test.exe")
	if err := Build(t.Path, exe); err != nil {
		return err
	}
	t.cmd = exec.Command(exe, "-network", "tcp", "-address", fmt.Sprintf(":%d", t.Config.MilterPort), "-tags", strings.Join(t.MTA.tags, " "))
	ctx, cancel := context.WithCancel(context.Background())
	t.wg.Add(1)
	go func() {
		b, err := t.cmd.CombinedOutput()
		t.m.Lock()
		t.startErr = err
		failedTest := t.failedTest
		t.m.Unlock()
		failed := !IsExpectedExitErr(err)
		if failed {
			LevelTwoLogger.Print(err)
		}
		if failed || failedTest {
			LevelTwoLogger.Printf("DIR %s\n%s", t.Path, b)
		}
		t.wg.Done()
		cancel()
	}()
	time.Sleep(time.Second)
	t.m.Lock()
	err = t.startErr
	t.m.Unlock()
	if err != nil {
		if e, ok := err.(*exec.ExitError); ok {
			if e.ExitCode() == integration.ExitSkip {
				return ErrTestSkipped
			}
		}
		return err
	}
	err = WaitForPort(ctx, t.Config.MilterPort)
	cancel()
	if err != nil {
		t.Stop()
		return err
	}
	return nil
}

func (t *TestDir) Stop() {
	t.once.Do(func() {
		if t.cmd != nil && t.cmd.Process != nil {
			t.cmd.Process.Signal(syscall.SIGTERM)
			t.cmd = nil
			t.wg.Wait()
		}
	})
}

func (t *TestDir) MarkFailedTest() {
	t.m.Lock()
	defer t.m.Unlock()
	t.failedTest = true
	t.MTA.MarkFailedTest()
}

type TestState int

const (
	TestReady TestState = iota
	TestOk
	TestSkipped
	TestFailed
)

type TestCase struct {
	Index    int
	Path     string
	Filename string
	TestCase *integration.TestCase
	smtpData bytes.Buffer
	Config   *Config
	parent   *TestDir
	State    TestState
}

func (t *TestCase) MarkFailed(format string, v ...any) {
	t.parent.MarkFailedTest()
	t.State = TestFailed
	LevelThreeLogger.Printf(format, v...)
	LevelThreeLogger.Printf("SMTP transaction:\n%s", t.smtpData.String())
}

func (t *TestCase) MarkSkipped(format string, v ...any) {
	LevelThreeLogger.Printf(format, v...)
	t.State = TestSkipped
}

func (t *TestCase) MarkOk(format string, v ...any) {
	LevelThreeLogger.Printf(format, v...)
	t.State = TestOk
}

type logWriter struct {
	t *TestCase
}

func (l *logWriter) Write(p []byte) (n int, err error) {
	return l.t.smtpData.Write(p)
}

func (t *TestCase) Send(steps []*integration.InputStep, port uint16) (uint16, string, integration.DecisionStep, error) {
	client, err := smtp.Dial(fmt.Sprintf(":%d", port))
	if err != nil {
		return 0, "", integration.StepAny, err
	}
	defer client.Close()
	client.DebugWriter = &logWriter{t: t}
	var dataWriter io.WriteCloser
	for _, step := range steps {
		switch step.What {
		case "HELO":
			if err := client.Hello(step.Arg); err != nil {
				return smtpErr(err, integration.StepHelo)
			}
		case "STARTTLS":
			if err := client.StartTLS(&tls.Config{InsecureSkipVerify: true}); err != nil {
				return smtpErr(err, integration.StepAny)
			}
		case "AUTH":
			password := "password1"
			if step.Arg == "user2@example.com" {
				password = "password2"
			}
			if err := client.Auth(sasl.NewPlainClient("", step.Arg, password)); err != nil {
				return smtpErr(err, integration.StepAny)
			}
		case "FROM":
			if err := client.Mail(step.Addr, nil); err != nil {
				return smtpErr(err, integration.StepFrom)
			}
		case "TO":
			if err := client.Rcpt(step.Addr); err != nil {
				return smtpErr(err, integration.StepTo)
			}
		case "RESET":
			if err := client.Reset(); err != nil {
				return smtpErr(err, integration.StepAny)
			}
		case "HEADER":
			dataWriter, err = client.Data()
			if err != nil {
				return smtpErr(err, integration.StepData)
			}
			if _, err := dataWriter.Write(step.Data); err != nil {
				return smtpErr(err, integration.StepAny)
			}
		case "BODY":
			if _, err := dataWriter.Write(step.Data); err != nil {
				return smtpErr(err, integration.StepAny)
			}
			if err := dataWriter.Close(); err != nil {
				return smtpErr(err, integration.StepEOM)
			}
			_ = client.Quit()
			return 250, "OK: queued", integration.StepEOM, nil
		default:
			return 0, "", integration.StepAny, fmt.Errorf("unknown step %s", step.What)
		}
	}
	return 0, "", integration.StepEOM, errors.New("incomplete input sequence")
}

func smtpErr(err error, step integration.DecisionStep) (uint16, string, integration.DecisionStep, error) {
	if sErr, ok := err.(*smtp.SMTPError); ok {
		return uint16(sErr.Code), sErr.Message, step, nil
	}
	return 0, "", step, err
}
