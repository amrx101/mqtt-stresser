package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

type PayloadGenerator func(i int) string

func defaultPayloadGen() PayloadGenerator {
	return func(i int) string {
		return fmt.Sprintf("this is msg #%d!", i)
	}
}

func constantPayloadGenerator(payload string) PayloadGenerator {
	return func(i int) string {
		return payload
	}
}

type Worker struct {
	WorkerId             int
	BrokerUrl            string
	Username             string
	Password             string
	SkipTLSVerification  bool
	NumberOfMessages     int
	PayloadGenerator     PayloadGenerator
	Timeout              time.Duration
	Retained             bool
	PublisherQoS         byte
	SubscriberQoS        byte
	CA                   []byte
	Cert                 []byte
	Key                  []byte
	PauseBetweenMessages time.Duration
}

func setSkipTLS(o *mqtt.ClientOptions) {
	oldTLSCfg := o.TLSConfig
	oldTLSCfg.InsecureSkipVerify = true
	o.SetTLSConfig(oldTLSCfg)
}

func NewTLSConfig(ca, certificate, privkey []byte) (*tls.Config, error) {
	// Import trusted certificates from CA
	caPem, _ := pem.Decode(ca)
	caCert, err := x509.ParseCertificate(caPem.Bytes)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	// CA v3 extensions missing. Wont verify server certs as
	// crypto module doesnt know
	caCert.BasicConstraintsValid = true
	caCert.IsCA = true
	caCert.KeyUsage = x509.KeyUsageCertSign

	certpool := x509.NewCertPool()
	certpool.AddCert(caCert)

	// certpool := x509.NewCertPool()
	// ok := certpool.AppendCertsFromPEM(ca)

	// if !ok {
	// 	return nil, fmt.Errorf("CA is invalid")
	// }

	// Import client certificate/key pair
	cert, err := tls.X509KeyPair(certificate, privkey)
	if err != nil {
		return nil, err
	}
	fmt.Println("Cert loaded")

	// Create tls.Config with desired tls properties
	return &tls.Config{
		// RootCAs = certs used to verify server cert.
		RootCAs: certpool,
		// ClientAuth = whether to request cert from server.
		// Since the server is set up for SSL, this happens
		// anyways.
		ClientAuth: tls.NoClientCert,
		// ClientCAs = certs used to validate client cert.
		ClientCAs: nil,
		// InsecureSkipVerify = verify that cert contents
		// match server. IP matches what is in cert etc.
		InsecureSkipVerify: false,
		// Certificates = list of certs client sends to server.
		Certificates: []tls.Certificate{cert},
	}, nil
}

func (w *Worker) Run(ctx context.Context) {
	verboseLogger.Printf("[%d] initializing\n", w.WorkerId)

	queue := make(chan [2]string)
	cid := w.WorkerId
	t := randomSource.Int31()

	hostname, err := os.Hostname()
	if err != nil {
		panic(err)
	}

	topicName := fmt.Sprintf(topicNameTemplate, hostname, w.WorkerId, t)
	subscriberClientId := fmt.Sprintf(subscriberClientIdTemplate, hostname, w.WorkerId, t)
	publisherClientId := fmt.Sprintf(publisherClientIdTemplate, hostname, w.WorkerId, t)

	verboseLogger.Printf("[%d] topic=%s subscriberClientId=%s publisherClientId=%s\n", cid, topicName, subscriberClientId, publisherClientId)

	publisherOptions := mqtt.NewClientOptions().SetClientID(publisherClientId).SetUsername(w.Username).SetPassword(w.Password).AddBroker(w.BrokerUrl)

	if w.SkipTLSVerification {
		setSkipTLS(publisherOptions)
	}

	subscriberOptions := mqtt.NewClientOptions().SetClientID(subscriberClientId).SetUsername(w.Username).SetPassword(w.Password).AddBroker(w.BrokerUrl)

	if w.SkipTLSVerification {
		setSkipTLS(subscriberOptions)
	}

	subscriberOptions.SetDefaultPublishHandler(func(client mqtt.Client, msg mqtt.Message) {
		queue <- [2]string{msg.Topic(), string(msg.Payload())}
	})

	if len(w.CA) > 0 || len(w.Key) > 0 {
		tlsConfig, err := NewTLSConfig(w.CA, w.Cert, w.Key)
		if err != nil {
			panic(err)
		}
		subscriberOptions.SetTLSConfig(tlsConfig)
		publisherOptions.SetTLSConfig(tlsConfig)
	}

	publisher := mqtt.NewClient(publisherOptions)
	subscriber := mqtt.NewClient(subscriberOptions)

	verboseLogger.Printf("[%d] connecting publisher\n", w.WorkerId)
	verboseLogger.Printf("%d timeout -> \n", w.Timeout)
	if token := publisher.Connect(); token.WaitTimeout(w.Timeout) && token.Error() != nil {
		resultChan <- Result{
			WorkerId:     w.WorkerId,
			Event:        ConnectFailedEvent,
			Error:        true,
			ErrorMessage: token.Error(),
		}
		return
	}
	verboseLogger.Printf("Connected Publisher")

	verboseLogger.Printf("[%d] connecting subscriber\n", w.WorkerId)
	if token := subscriber.Connect(); token.WaitTimeout(w.Timeout) && token.Error() != nil {
		resultChan <- Result{
			WorkerId:     w.WorkerId,
			Event:        ConnectFailedEvent,
			Error:        true,
			ErrorMessage: token.Error(),
		}

		return
	}

	defer func() {
		verboseLogger.Printf("[%d] unsubscribe\n", w.WorkerId)

		if token := subscriber.Unsubscribe(topicName); token.WaitTimeout(w.Timeout) && token.Error() != nil {
			fmt.Println(token.Error())
			os.Exit(1)
		}

		subscriber.Disconnect(5)
	}()

	verboseLogger.Printf("[%d] subscribing to topic\n", w.WorkerId)
	if token := subscriber.Subscribe(topicName, w.SubscriberQoS, nil); token.WaitTimeout(w.Timeout) && token.Error() != nil {
		resultChan <- Result{
			WorkerId:     w.WorkerId,
			Event:        SubscribeFailedEvent,
			Error:        true,
			ErrorMessage: token.Error(),
		}

		return
	}

	verboseLogger.Printf("[%d] starting control loop %s\n", w.WorkerId, topicName)

	stopWorker := false
	receivedCount := 0
	publishedCount := 0

	t0 := time.Now()
	for i := 0; i < w.NumberOfMessages; i++ {
		text := w.PayloadGenerator(i)
		token := publisher.Publish(topicName, w.PublisherQoS, w.Retained, text)
		publishedCount++
		token.WaitTimeout(w.Timeout)
		time.Sleep(w.PauseBetweenMessages)
	}
	publisher.Disconnect(5)

	publishTime := time.Since(t0)
	verboseLogger.Printf("[%d] all messages published\n", w.WorkerId)

	t0 = time.Now()
	for receivedCount < w.NumberOfMessages && !stopWorker {
		select {
		case <-queue:
			receivedCount++

			verboseLogger.Printf("[%d] %d/%d received\n", w.WorkerId, receivedCount, w.NumberOfMessages)
			if receivedCount == w.NumberOfMessages {
				resultChan <- Result{
					WorkerId:          w.WorkerId,
					Event:             CompletedEvent,
					PublishTime:       publishTime,
					ReceiveTime:       time.Since(t0),
					MessagesReceived:  receivedCount,
					MessagesPublished: publishedCount,
				}
			} else {
				resultChan <- Result{
					WorkerId:          w.WorkerId,
					Event:             ProgressReportEvent,
					PublishTime:       publishTime,
					ReceiveTime:       time.Since(t0),
					MessagesReceived:  receivedCount,
					MessagesPublished: publishedCount,
				}
			}
		case <-ctx.Done():
			var event string
			var isError bool
			switch ctx.Err().(type) {
			case TimeoutError:
				verboseLogger.Printf("[%d] received abort signal due to test timeout", w.WorkerId)
				event = TimeoutExceededEvent
				isError = true
			default:
				verboseLogger.Printf("[%d] received abort signal", w.WorkerId)
				event = AbortedEvent
				isError = false
			}
			stopWorker = true
			resultChan <- Result{
				WorkerId:          w.WorkerId,
				Event:             event,
				PublishTime:       publishTime,
				MessagesReceived:  receivedCount,
				MessagesPublished: publishedCount,
				Error:             isError,
			}
		}
	}

	verboseLogger.Printf("[%d] worker finished\n", w.WorkerId)
}
