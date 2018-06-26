// Copyright 2018 Kaleido, a ConsenSys business

// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at

//     http://www.apache.org/licenses/LICENSE-2.0

// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package kldkafka

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Shopify/sarama"
	cluster "github.com/bsm/sarama-cluster"
	"github.com/kaleido-io/ethconnect/internal/kldmessages"
	"github.com/kaleido-io/ethconnect/internal/kldutils"
	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
)

type testKafkaFactory struct {
	errorOnNewClient   error
	errorOnNewProducer error
	errorOnNewConsumer error
	producer           *testKafkaProducer
	consumer           *testKafkaConsumer
	processor          *testKafkaMsgProcessor
}

var minWorkingArgs = []string{
	"-r", "https://testrpc.example.com",
	"-t", "in-topic",
	"-T", "out-topic",
	"-g", "test-group",
}

var testClientConf *cluster.Config

func (f *testKafkaFactory) newClient(k *KafkaBridge, clientConf *cluster.Config) (kafkaClient, error) {
	testClientConf = clientConf
	return f, f.errorOnNewClient
}

func (f *testKafkaFactory) Brokers() []*sarama.Broker {
	return []*sarama.Broker{}
}

func (f *testKafkaFactory) newProducer(k *KafkaBridge) (kafkaProducer, error) {
	f.producer = &testKafkaProducer{
		input:     make(chan *sarama.ProducerMessage),
		successes: make(chan *sarama.ProducerMessage),
		errors:    make(chan *sarama.ProducerError),
	}
	return f.producer, f.errorOnNewProducer
}

func (f *testKafkaFactory) newConsumer(k *KafkaBridge) (kafkaConsumer, error) {
	f.consumer = &testKafkaConsumer{
		messages:      make(chan *sarama.ConsumerMessage),
		notifications: make(chan *cluster.Notification),
		errors:        make(chan error),
	}
	return f.consumer, f.errorOnNewConsumer
}

type testKafkaProducer struct {
	input     chan *sarama.ProducerMessage
	successes chan *sarama.ProducerMessage
	errors    chan *sarama.ProducerError
}

func (p testKafkaProducer) Close() error {
	if p.input != nil {
		close(p.input)
	}
	if p.successes != nil {
		close(p.successes)
	}
	if p.errors != nil {
		close(p.errors)
	}
	return nil
}

func (p testKafkaProducer) Input() chan<- *sarama.ProducerMessage {
	return p.input
}

func (p testKafkaProducer) Successes() <-chan *sarama.ProducerMessage {
	return p.successes
}

func (p testKafkaProducer) Errors() <-chan *sarama.ProducerError {
	return p.errors
}

type testKafkaConsumer struct {
	messages      chan *sarama.ConsumerMessage
	notifications chan *cluster.Notification
	errors        chan error
}

func (c testKafkaConsumer) Close() error {
	if c.messages != nil {
		close(c.messages)
	}
	if c.notifications != nil {
		close(c.notifications)
	}
	if c.errors != nil {
		close(c.errors)
	}
	return nil
}

func (c testKafkaConsumer) Messages() <-chan *sarama.ConsumerMessage {
	return c.messages
}

func (c testKafkaConsumer) Notifications() <-chan *cluster.Notification {
	return c.notifications
}

func (c testKafkaConsumer) Errors() <-chan error {
	return c.errors
}

func (c testKafkaConsumer) MarkOffset(*sarama.ConsumerMessage, string) {
	return
}

type testKafkaMsgProcessor struct {
	messages chan MsgContext
}

func (p *testKafkaMsgProcessor) OnMessage(msg MsgContext) (err error) {
	log.Infof("Dispatched message context to processor: %s", msg)
	p.messages <- msg
	return
}

func startTestBridge(assert *assert.Assertions, testArgs []string, f *testKafkaFactory) (k *KafkaBridge, wg *sync.WaitGroup, err error) {
	k = NewKafkaBridge()
	k.factory = f
	k.processor = f.processor
	kafkaCmd := k.CobraInit()
	kafkaCmd.SetArgs(testArgs)

	wg = &sync.WaitGroup{}
	wg.Add(1)
	go func() {
		err = kafkaCmd.Execute()
		wg.Done()
	}()
	for k.signals == nil && err == nil {
		time.Sleep(10 * time.Millisecond)
	}
	return k, wg, err
}

// execBridgeWithArgs is a helper that runs the KafkaBridge with the specified
// commandline (and stubbed kafka impl), waits for it to initialize, then
// terminates it
func execBridgeWithArgs(assert *assert.Assertions, testArgs []string, f *testKafkaFactory) (k *KafkaBridge, err error) {

	k, wg, err := startTestBridge(assert, testArgs, f)
	if err == nil {
		k.signals <- os.Interrupt
	}
	wg.Wait()

	return
}

func TestNewKafkaBridge(t *testing.T) {
	assert := assert.New(t)

	bridge := NewKafkaBridge()

	assert.NotNil(bridge.inFlight)
	assert.NotNil(bridge.inFlightCond)
}

func TestExecuteWithIncompleteArgs(t *testing.T) {
	assert := assert.New(t)

	var k KafkaBridge
	var f testKafkaFactory
	k.factory = &f

	kafkaCmd := k.CobraInit()

	testArgs := []string{}

	kafkaCmd.SetArgs(testArgs)
	err := kafkaCmd.Execute()
	assert.Equal(err.Error(), "No output topic specified for bridge to send events to")
	testArgs = append(testArgs, []string{"-T", "test"}...)

	kafkaCmd.SetArgs(testArgs)
	err = kafkaCmd.Execute()
	assert.Equal(err.Error(), "No input topic specified for bridge to listen to")
	testArgs = append(testArgs, []string{"-t", "test-in"}...)

	kafkaCmd.SetArgs(testArgs)
	err = kafkaCmd.Execute()
	assert.Equal(err.Error(), "No consumer group specified")
	testArgs = append(testArgs, []string{"-g", "test-group"}...)

	kafkaCmd.SetArgs(testArgs)
	err = kafkaCmd.Execute()
	assert.Equal(err.Error(), "No JSON/RPC URL set for ethereum node")
	testArgs = append(testArgs, []string{"-r", "http://localhost:8545"}...)

	testArgs = append(testArgs, []string{"--tls-clientcerts", "/some/file"}...)
	kafkaCmd.SetArgs(testArgs)
	err = kafkaCmd.Execute()
	assert.Equal("flag mismatch: 'tls-clientcerts' set and 'tls-clientkey' unset", err.Error())
	testArgs = append(testArgs, []string{"--tls-clientkey", "somekey"}...)

	testArgs = append(testArgs, []string{"--sasl-username", "testuser"}...)
	kafkaCmd.SetArgs(testArgs)
	err = kafkaCmd.Execute()
	assert.Equal("flag mismatch: 'sasl-username' set and 'sasl-password' unset", err.Error())
	testArgs = append(testArgs, []string{"--sasl-password", "testpass"}...)
}

func TestExecuteWithBadRPCURL(t *testing.T) {
	assert := assert.New(t)

	_, err := execBridgeWithArgs(assert, []string{
		"-r", "!!!bad!!!",
		"-t", "in-topic",
		"-T", "out-topic",
		"-g", "test-group",
		"-u", "testuser",
		"-p", "testpass",
	}, &testKafkaFactory{})

	assert.Regexp("connect", err.Error())

}
func TestExecuteWithNewClientError(t *testing.T) {
	assert := assert.New(t)

	_, err := execBridgeWithArgs(assert, minWorkingArgs, &testKafkaFactory{
		errorOnNewClient: fmt.Errorf("pop"),
	})

	assert.Regexp("pop", err.Error())

}

func TestExecuteWithNewProducerError(t *testing.T) {
	assert := assert.New(t)

	_, err := execBridgeWithArgs(assert, minWorkingArgs, &testKafkaFactory{
		errorOnNewProducer: fmt.Errorf("fizz"),
	})

	assert.Regexp("fizz", err.Error())

}
func TestExecuteWithNewConsumerError(t *testing.T) {
	assert := assert.New(t)

	_, err := execBridgeWithArgs(assert, minWorkingArgs, &testKafkaFactory{
		errorOnNewConsumer: fmt.Errorf("bang"),
	})

	assert.Regexp("bang", err.Error())

}
func TestExecuteWithNoTLS(t *testing.T) {
	assert := assert.New(t)

	_, err := execBridgeWithArgs(assert, minWorkingArgs, &testKafkaFactory{})

	assert.Equal(nil, err)
	assert.Equal(true, testClientConf.Producer.Return.Successes)
	assert.Equal(true, testClientConf.Producer.Return.Errors)
	assert.Equal(sarama.WaitForLocal, testClientConf.Producer.RequiredAcks)
	assert.Equal(500*time.Millisecond, testClientConf.Producer.Flush.Frequency)
	assert.Equal(true, testClientConf.Consumer.Return.Errors)
	assert.Equal(true, testClientConf.Group.Return.Notifications)
	assert.Equal(false, testClientConf.Net.TLS.Enable)
	assert.Equal((*tls.Config)(nil), testClientConf.Net.TLS.Config)
	assert.Regexp("\\w+", testClientConf.ClientID) // generated UUID

}

func TestExecuteWithSASL(t *testing.T) {
	assert := assert.New(t)

	_, err := execBridgeWithArgs(assert, []string{
		"-r", "https://testrpc.example.com",
		"-t", "in-topic",
		"-T", "out-topic",
		"-g", "test-group",
		"-u", "testuser",
		"-p", "testpass",
	}, &testKafkaFactory{})

	assert.Equal(nil, err)
	assert.Equal("testuser", testClientConf.Net.SASL.User)
	assert.Equal("testpass", testClientConf.Net.SASL.Password)

}

func TestExecuteWithDefaultTLSAndClientID(t *testing.T) {
	assert := assert.New(t)

	_, err := execBridgeWithArgs(assert, []string{
		"-r", "https://testrpc.example.com",
		"-t", "in-topic",
		"-T", "out-topic",
		"-g", "test-group",
		"-i", "clientid1",
		"--tls-enabled",
	}, &testKafkaFactory{})

	assert.Equal(nil, err)
	assert.Equal(true, testClientConf.Net.TLS.Enable)
	assert.Equal(0, len(testClientConf.Net.TLS.Config.Certificates))
	assert.Equal((*x509.CertPool)(nil), testClientConf.Net.TLS.Config.RootCAs)
	assert.Equal(false, testClientConf.Net.TLS.Config.InsecureSkipVerify)
	assert.Equal("clientid1", testClientConf.ClientID) // generated UUID

}

func TestExecuteWithSelfSignedMutualAuth(t *testing.T) {
	assert := assert.New(t)

	testPrivKeyFile, _ := ioutil.TempFile("", "testprivkey")
	defer syscall.Unlink(testPrivKeyFile.Name())
	ioutil.WriteFile(testPrivKeyFile.Name(), []byte(
		"-----BEGIN PRIVATE KEY-----\n"+
			"MIIEvgIBADANBgkqhkiG9w0BAQEFAASCBKgwggSkAgEAAoIBAQDXCA6IdwIgFeRw\n"+
			"MSEJSbKWtujq4+8DUEZIYRLbYXq2oblrK6ObmtkdBOUktDGeVbeaffEHJsqN5pYE\n"+
			"n9WBMdxxYkQtov5DX8Gbmx8IUr74NtosCG2jWW5yVjdMYhf8IGPUQsV6Za5L2VBX\n"+
			"o4kd6wA25AKYz+xao0WFxHiqDTJteWGvl+e44IinrQ2yTfcB43PjYUcqav4VO1dt\n"+
			"VrDSMcVs6vtXLzX4sFewqudrACirJOgtZs5kfLYQO5coTN35mUIeY8eRUtWapq8e\n"+
			"bhtTbO4gxOyfwJ3Ck2A3mk6TcZYLT/g+6X8FxohGzymbBYTr7HXb6f/GC1DQu0hg\n"+
			"SP98+y1pAgMBAAECggEAQY1wOMPm/vcNk/I2Owmfivip2umvtJflRS1qvTxjV4fH\n"+
			"6db84nP7WjBi1qSkN7uz5EIel2qI92djNnevc9pKdLpbRHpa/xkTAafxdu0a0LqQ\n"+
			"Gjpbih+6XtrPstZ4r2EEbfIJF74lu3O9XWo6Y8d/YjxyWjmQuTTq/dOeYWDyjZKT\n"+
			"WgpaneD2vxRAwvUCaypvGNs20FjX7MhgB7Xj2MDg3t04C10s/G7IGyncuZfrnZU9\n"+
			"LA70aZEaW/TkbWyzEMr4ObjjkIO0Xlr1pp1fNIGIFc39tzQoKDhLP/0t4MFXrgm8\n"+
			"/ff+sW7pNd2sbZHaUPTzBYEa/D4XcLVqt/479F4SHQKBgQDsEzVBFyWCBJ2UiCYw\n"+
			"jmMS8jURi9k5DuwSUOPGkJgRI0U6uyXh6s8u9UHPbgKhMf9IUs2fMaGG338tDRyP\n"+
			"SDT5x84jCoIKjj9CXKHbsfmHW6EzHJqXgYX3rnv19cwpJ2a1Wryoljca+zecwpUX\n"+
			"CNXTm3oRGj6aim8VYRcyaQfEdwKBgQDpLir0iNoq3CuGJFqTYydFQuSWIs8DszXm\n"+
			"12k4kFe0+6ANM2yxM0JkvPljLz/Pfb+1mDKK5dqBlILZ/T3100hpvzEueJoeoT2q\n"+
			"a7rF9+6iOF62DNfyis0DxFmB8KYf4GUaWnGI7z0UVZaFdjhvl/xATMMcaSFZVA+m\n"+
			"cNm42cB1HwKBgDGwMUtL9ecR1aEHrxIVRiEcvbK9vrDVxTZttCN9F6SzycR805Jj\n"+
			"e8wkbv+b5g3LmjG8y+6v4ZGjxP7UfahiyFOyjF6vvYM/QW1UVfUJ1r14ucsqQBeX\n"+
			"eX0SSqEQZTJcSq/tMzxAscSKD8B87Ch3AZqSZPTokziv3oWfc+R2Wt4tAoGBAILa\n"+
			"Np69QXi1zvLa6b018i6q6C3cYMFZyxC8pz5nueBFKD7gMcmK02JGrchcFnnwvilA\n"+
			"vHQ3opP+7CM6Oo/9vfAhq47BfPNdVoaRJ+G6TT7ZVUTiFjj0bTIE+JmzmvXebb4J\n"+
			"LRdD8cm8cdh5TBhLePH4YbFKyb0gMBwdzgAuqhLPAoGBAMuMa5gEGNvUMRA5hymp\n"+
			"IKMoO01vmc7c8gJmunFujMZS1fQQA2Qzw16z6ekZgLVgLz27ivfh203qgqORWyXg\n"+
			"HvrK9nTzXLEnYprg0bmyivjo0LAk0ISuAdTSWGWhUZqLON8g5T9AmCGmtFE6FZEl\n"+
			"YV406bLxh5diFcQIEFGfgWNe\n"+
			"-----END PRIVATE KEY-----\n"), 0644)

	testCertData := []byte(
		"-----BEGIN CERTIFICATE-----\n" +
			"MIIDYjCCAkoCCQCl+tdkvcUkzTANBgkqhkiG9w0BAQsFADBzMQswCQYDVQQGEwJV\n" +
			"UzELMAkGA1UECAwCTkMxEDAOBgNVBAcMB1JhbGVpZ2gxEDAOBgNVBAoMB0thbGVp\n" +
			"ZG8xFTATBgNVBAsMDFVuaXQgdGVzdGluZzEcMBoGA1UEAwwTdW5pdHRlc3RAa2Fs\n" +
			"ZWlkby5pbzAeFw0xODA2MjUxOTAzMzJaFw0xODA3MjUxOTAzMzJaMHMxCzAJBgNV\n" +
			"BAYTAlVTMQswCQYDVQQIDAJOQzEQMA4GA1UEBwwHUmFsZWlnaDEQMA4GA1UECgwH\n" +
			"S2FsZWlkbzEVMBMGA1UECwwMVW5pdCB0ZXN0aW5nMRwwGgYDVQQDDBN1bml0dGVz\n" +
			"dEBrYWxlaWRvLmlvMIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEA1wgO\n" +
			"iHcCIBXkcDEhCUmylrbo6uPvA1BGSGES22F6tqG5ayujm5rZHQTlJLQxnlW3mn3x\n" +
			"BybKjeaWBJ/VgTHccWJELaL+Q1/Bm5sfCFK++DbaLAhto1luclY3TGIX/CBj1ELF\n" +
			"emWuS9lQV6OJHesANuQCmM/sWqNFhcR4qg0ybXlhr5fnuOCIp60Nsk33AeNz42FH\n" +
			"Kmr+FTtXbVaw0jHFbOr7Vy81+LBXsKrnawAoqyToLWbOZHy2EDuXKEzd+ZlCHmPH\n" +
			"kVLVmqavHm4bU2zuIMTsn8CdwpNgN5pOk3GWC0/4Pul/BcaIRs8pmwWE6+x12+n/\n" +
			"xgtQ0LtIYEj/fPstaQIDAQABMA0GCSqGSIb3DQEBCwUAA4IBAQDSVZLxNLrsuciQ\n" +
			"NIxbaBhjpilrOvGheKNZH6cSscPhfqyLSLrx1BumgB8Bp2aCxTv9zDh4ugUhrkEz\n" +
			"babAZJAlIfSD3IdwVFR4O2FBOLn73Ql1xoTqN1S2tersLzRy87BfDWxNIMQzwK5U\n" +
			"3I+xwCPCbtBrxZPULXT+fBlZjwCgC0MdKgq3aMsPLlPawSk1sT8BvQrn3o7dSe8q\n" +
			"kAhSssaP9XJDoV6saPMzjb+WUNZgI3uTw3nxbjr+rIM+C2KvPGS/+lpFfpGg0DMf\n" +
			"+eHpZMb2Vf1HzDxM1KGkpDI2McyVF6OxHJcITPY2GG2FKMnxg5Zj3Euzs8FDcg62\n" +
			"IjUBP/mt\n" +
			"-----END CERTIFICATE-----\n")
	testCertFile, _ := ioutil.TempFile("", "testcert")
	defer syscall.Unlink(testCertFile.Name())
	ioutil.WriteFile(testCertFile.Name(), testCertData, 0644)
	testCACertFile, _ := ioutil.TempFile("", "testca")
	defer syscall.Unlink(testCACertFile.Name())
	ioutil.WriteFile(testCACertFile.Name(), testCertData, 0644)

	mutualTLSArgs := []string{
		"-r", "https://testrpc.example.com",
		"-t", "in-topic",
		"-T", "out-topic",
		"-g", "test-group",
		"-i", "clientid1",
		"--tls-enabled",
		"--tls-insecure",
		"--tls-clientkey", testPrivKeyFile.Name(),
		"--tls-clientcerts", testCertFile.Name(),
		"--tls-cacerts", testCACertFile.Name(),
	}

	k, err := execBridgeWithArgs(assert, mutualTLSArgs, &testKafkaFactory{})

	assert.Equal(nil, err)
	assert.Equal(true, testClientConf.Net.TLS.Enable)
	assert.Equal(1, len(testClientConf.Net.TLS.Config.Certificates))
	assert.Equal(1, len(testClientConf.Net.TLS.Config.RootCAs.Subjects()))
	assert.Equal(true, testClientConf.Net.TLS.Config.InsecureSkipVerify)
	assert.Equal("clientid1", testClientConf.ClientID) // generated UUID

	// Validate error handling
	syscall.Unlink(testCACertFile.Name())
	kafkaCmd := k.CobraInit()
	kafkaCmd.SetArgs(mutualTLSArgs)
	err = kafkaCmd.Execute()
	assert.Regexp("no such file or directory", err.Error())

	syscall.Unlink(testCertFile.Name())
	kafkaCmd = k.CobraInit()
	kafkaCmd.SetArgs(mutualTLSArgs)
	err = kafkaCmd.Execute()
	assert.Regexp("no such file or directory", err.Error())
}

func TestPrintForSaramaLogger(t *testing.T) {
	var l saramaLogger
	l.Print("Test log")
}

func TestPrintfForSaramaLogger(t *testing.T) {
	var l saramaLogger
	l.Printf("Test log")
}

func TestPrintlnForSaramaLogger(t *testing.T) {
	var l saramaLogger
	l.Println("Test log")
}

func TestAddInflightMsg(t *testing.T) {
	assert := assert.New(t)

	f := testKafkaFactory{
		processor: &testKafkaMsgProcessor{
			messages: make(chan MsgContext),
		},
	}
	k, wg, err := startTestBridge(assert, minWorkingArgs, &f)
	if err != nil {
		return
	}

	// Send a minimal test message
	msg1 := kldmessages.RequestCommon{}
	msg1.Headers.MsgType = "TestAddInflightMsg"
	msg1.Headers.ID = kldutils.UUIDv4()
	msg1Ctx := struct {
		Some string `json:"some"`
	}{
		Some: "data",
	}
	msg1.Headers.Context = &msg1Ctx
	msg1bytes, err := json.Marshal(&msg1)
	log.Infof("Sent message: %s", string(msg1bytes))
	f.consumer.messages <- &sarama.ConsumerMessage{
		Value: msg1bytes,
	}

	msgContext1 := <-f.processor.messages
	assert.Equal(msg1.Headers.ID, msgContext1.Headers().ID)
	assert.Equal(msg1.Headers.MsgType, msgContext1.Headers().MsgType)
	assert.Equal("data", msgContext1.Headers().Context.(map[string]interface{})["some"])

	// Shut down
	k.signals <- os.Interrupt
	wg.Wait()
}
