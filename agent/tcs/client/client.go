// Copyright 2014-2015 Amazon.com, Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//	http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package tcsclient

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/aws/amazon-ecs-agent/agent/ecs_client/authv4"
	"github.com/aws/amazon-ecs-agent/agent/ecs_client/authv4/credentials"
	"github.com/aws/amazon-ecs-agent/agent/logger"
	"github.com/aws/amazon-ecs-agent/agent/stats"
	"github.com/aws/amazon-ecs-agent/agent/tcs/model/ecstcs"
	"github.com/aws/amazon-ecs-agent/agent/wsclient"
	"github.com/gorilla/websocket"
)

// tasksInMessage is the maximum number of tasks that can be sent in a message to the backend
// This is a very conservative estimate assuming max allowed string lengths for all fields.
const tasksInMessage = 10

var log = logger.ForModule("tcs client")

// clientServer implements wsclient.ClientServer interface for metrics backend.
type clientServer struct {
	statsEngine            stats.Engine
	publishTicker          *time.Ticker
	publishMetricsInterval time.Duration
	wsclient.ClientServerImpl
	signer authv4.HttpSigner
}

// New returns a client/server to bidirectionally communicate with the backend.
// The returned struct should have both 'Connect' and 'Serve' called upon it
// before being used.
func New(url string, region string, credentialProvider credentials.AWSCredentialProvider, acceptInvalidCert bool, statsEngine stats.Engine, publishMetricsInterval time.Duration) wsclient.ClientServer {
	cs := &clientServer{
		statsEngine:            statsEngine,
		publishTicker:          nil,
		publishMetricsInterval: publishMetricsInterval,
		signer:                 authv4.NewHttpSigner(region, wsclient.ServiceName, credentialProvider, nil),
	}
	cs.URL = url
	cs.Region = region
	cs.CredentialProvider = credentialProvider
	cs.AcceptInvalidCert = acceptInvalidCert
	cs.ServiceError = &tcsError{}
	cs.RequestHandlers = make(map[string]wsclient.RequestHandler)
	cs.TypeDecoder = &TcsDecoder{}
	return cs
}

// Serve begins serving requests using previously registered handlers (see
// AddRequestHandler). All request handlers should be added prior to making this
// call as unhandled requests will be discarded.
func (cs *clientServer) Serve() error {
	log.Debug("Starting websocket poll loop")
	if cs.Conn == nil {
		return fmt.Errorf("nil connection")
	}

	if cs.statsEngine == nil {
		return fmt.Errorf("uninitialized stats engine")
	}

	// Start the timer function to publish metrics to the backend.
	cs.publishTicker = time.NewTicker(cs.publishMetricsInterval)
	go cs.publishMetrics()

	return cs.ConsumeMessages()
}

// MakeRequest makes a request using the given input. Note, the input *MUST* be
// a pointer to a valid backend type that this client recognises
func (cs *clientServer) MakeRequest(input interface{}) error {
	payload, err := cs.CreateRequestMessage(input)
	if err != nil {
		return err
	}

	log.Debug("sending payload", "payload", string(payload))
	data := cs.signRequest(payload)

	// Over the wire we send something like
	// {"type":"AckRequest","message":{"messageId":"xyz"}}
	return cs.Conn.WriteMessage(websocket.TextMessage, data)
}

func (cs *clientServer) signRequest(payload []byte) []byte {
	signer := authv4.NewHttpSigner(cs.Region, "ecs", cs.CredentialProvider, nil)
	reqBody := bytes.NewBuffer(payload)
	// NewRequest never returns an error if the url parses and we just verified
	// it did above
	request, _ := http.NewRequest("GET", cs.URL, reqBody)
	signer.SignHttpRequest(request)

	var data []byte
	for k, vs := range request.Header {
		for _, v := range vs {
			data = append(data, k...)
			data = append(data, ": "...)
			data = append(data, v...)
			data = append(data, "\r\n"...)
		}
	}
	data = append(data, "\r\n"...)
	data = append(data, payload...)

	return data
}

// Close closes the underlying connection.
func (cs *clientServer) Close() error {
	if cs.publishTicker != nil {
		cs.publishTicker.Stop()
	}
	if cs.Conn != nil {
		return cs.Conn.Close()
	}
	return errors.New("No connection to close")
}

// publishMetrics invokes the PublishMetricsRequest on the clientserver object.
func (cs *clientServer) publishMetrics() {
	if cs.publishTicker == nil {
		log.Debug("publish ticker uninitialized")
		return
	}

	// Publish metrics immediately after we connect and wait for ticks. This makes
	// sure that there is no data loss when a scheduled metrics publishing fails
	// due to a connection reset.
	cs.publishMetricsOnce()
	for range cs.publishTicker.C {
		cs.publishMetricsOnce()
	}
}

// publishMetricsOnce is invoked by the ticker to periodically publish metrics to backend.
func (cs *clientServer) publishMetricsOnce() {
	metadata, taskMetrics, err := cs.statsEngine.GetInstanceMetrics()
	if err != nil {
		log.Warn("Error getting instance metrics", "err", err)
		return
	}

	if *metadata.Idle {
		// Idle instance, send message and return.
		cs.MakeRequest(ecstcs.NewPublishMetricsRequest(metadata, taskMetrics))
		return
	}

	var messageTaskMetrics []*ecstcs.TaskMetric
	for i := range taskMetrics {
		messageTaskMetrics = append(messageTaskMetrics, taskMetrics[i])
		if (i+1)%tasksInMessage == 0 {
			// Construct payload with tasksInMessage number of task metrics and send to backend.
			cs.MakeRequest(ecstcs.NewPublishMetricsRequest(metadata, messageTaskMetrics))
			messageTaskMetrics = messageTaskMetrics[:0]
		}
	}

	if len(messageTaskMetrics) > 0 {
		// Send remaining task metrics to backend.
		cs.MakeRequest(ecstcs.NewPublishMetricsRequest(metadata, messageTaskMetrics))
	}
}
