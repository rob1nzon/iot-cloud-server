package broker

import (
	"context"
	"encoding/json"
	"io"
	"sync"
	"time"

	"github.com/AcroManiac/iot-cloud-server/internal/domain/logic/messages"
	"github.com/AcroManiac/iot-cloud-server/internal/domain/logic/tasks"
	"github.com/pkg/errors"

	"github.com/AcroManiac/iot-cloud-server/internal/infrastructure/database"

	"github.com/AcroManiac/iot-cloud-server/internal/domain/entities"
	"github.com/AcroManiac/iot-cloud-server/internal/domain/interfaces"
	"github.com/AcroManiac/iot-cloud-server/internal/domain/logic"
	"github.com/AcroManiac/iot-cloud-server/internal/infrastructure/logger"

	"github.com/streadway/amqp"
)

type GatewayChannel struct {
	serverId  string
	gatewayId string
	conn      *database.Connection
	out       io.ReadCloser
	in        io.WriteCloser
	ctx       context.Context
	cancel    context.CancelFunc
	bl        interfaces.Logic
}

func NewGatewayChannel(amqpConn *amqp.Connection, dbConn *database.Connection, serverId, gatewayId string) interfaces.Channel {
	// Create cancel context
	ctx, cancel := context.WithCancel(context.Background())

	// Create gateway reader and writer
	out := NewAmqpReader(ctx, amqpConn, gatewayId)
	if out == nil {
		return nil
	}
	in := NewAmqpWriter(amqpConn, gatewayId)
	if in == nil {
		return nil
	}

	return &GatewayChannel{
		serverId:  serverId,
		gatewayId: gatewayId,
		conn:      dbConn,
		out:       out,
		in:        in,
		ctx:       ctx,
		cancel:    cancel,
		bl:        nil, // Do not create business logic until gateway status come
	}
}

func (c *GatewayChannel) Read(p []byte) (n int, err error) {
	return c.out.Read(p)
}

func (c *GatewayChannel) Write(p []byte) (n int, err error) {
	return c.in.Write(p)
}

func (c *GatewayChannel) Close() error {
	c.Stop()
	return c.out.Close()
}

func (c GatewayChannel) PrintMessage(message entities.IotMessage) {
	// Slim long preview
	if len(message.Preview) > 0 {
		message.Preview = "Some Base64 code ;)"
	}
	logger.Debug("Message from gateway", "message", message, "gateway", c.gatewayId)
}

func (c *GatewayChannel) Start() {
	// Read and process messages from gateway
	go func() {
		var mx sync.Mutex
		buffer := make([]byte, 50*1024)
	OUTER:
		for {
			select {
			case <-c.ctx.Done():
				break OUTER
			default:
				mx.Lock()
				length, err := c.Read(buffer)
				mx.Unlock()
				if err != nil {
					logger.Error("error reading channel", "error", err)
					continue
				}

				// Start processing incoming message in a separate goroutine
				go func() {
					iotmessage := &entities.IotMessage{}
					mx.Lock()
					err := json.Unmarshal(buffer[:length], iotmessage)
					mx.Unlock()
					if err != nil {
						logger.Error("can not unmarshal incoming gateway message",
							"error", err,
							"gateway", c.gatewayId)
						return
					}
					// Print copy of incoming message to log
					c.PrintMessage(*iotmessage)
					// Load business logic if gateway is online and registered in database
					if c.bl == nil {
						exists, err := c.CheckGatewayExistence(iotmessage)
						if err != nil {
							logger.Error("error checking gateway in database",
								"error", err,
								"gateway", c.gatewayId,
								"caller", "GatewayChannel")
							return
						}
						if !exists {
							logger.Warn("Gateway is not registered in cloud database",
								"gateway", c.gatewayId,
								"caller", "GatewayChannel")
							return
						}

						// Create business logic
						c.bl, err = c.CreateLogic()
						if err != nil {
							logger.Error("cannot load business logic params",
								"error", err,
								"gateway", c.gatewayId,
								"caller", "GatewayChannel")
						}
					}

					// Process incoming message
					if err := c.bl.Process(iotmessage); err != nil {
						logger.Error("error processing message",
							"error", err,
							"gateway", c.gatewayId,
							"caller", "GatewayChannel")
					}
				}()
			}
		}
	}()
}

// Function creates business logic and loads params
func (c *GatewayChannel) CreateLogic() (interfaces.Logic, error) {
	bl := logic.NewGatewayLogic(c.ctx, c.conn, c.gatewayId)
	if err := bl.LoadParams(c.in); err != nil {
		return nil, err
	}
	return bl, nil
}

// Check for gateway records in database and update devices statuses
func (c *GatewayChannel) CheckGatewayExistence(message *entities.IotMessage) (bool, error) {
	// Search gateway in database
	ctx, _ := context.WithTimeout(context.Background(), 5*time.Second)
	queryText := `select count(*) from v3_gateways where gateway_id = ?`
	var value int
	err := c.conn.Db.QueryRowContext(ctx, queryText, message.GatewayId).Scan(&value)
	if err != nil {
		return false, errors.Wrap(err, "failed searching gateway in database")
	}
	if value == 0 {
		// No gateway found
		return false, nil
	}

	// Found gateway in database
	return true, nil
}

func (c *GatewayChannel) Stop() {
	// Stop goroutines - fire context cancelling
	c.cancel()

	// Close i/o channels
	if err := c.out.Close(); err != nil {
		logger.Error("error closing gateway output channel",
			"error", err, "caller", "GatewayChannel")
	}
	if err := c.in.Close(); err != nil {
		logger.Error("error closing gateway input channel",
			"error", err, "caller", "GatewayChannel")
	}

	// Change gateway and all its devices statuses to offline in database
	statusMessage := messages.NewStatusMessage(c.gatewayId, "off")
	tasks.NewUpdateGatewayStatusTask(c.conn).Run(statusMessage)
}
