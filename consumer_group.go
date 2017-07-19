package consumergroup

import (
	"errors"
	"fmt"
	"runtime/debug"
	"sync"
	"time"

	"github.com/Shopify/sarama"
	"github.com/samuel/go-zookeeper/zk"
)

// Constants defining consumer group's possible states.
const (
	cgInit = iota
	cgStart
	cgStopped
)

type topicOffset map[int32]int64

// ConsumerGroup process Kafka messages from brokers. It supports group
// and rebalance.
type ConsumerGroup struct {
	name           string
	topicList      []string
	storage        groupStorage
	saramaConsumer sarama.Consumer

	id               string
	state            int
	stopper          chan struct{}
	rebalanceTrigger chan struct{}
	stopOnce         *sync.Once
	rebalanceOnce    *sync.Once

	nextMessage map[string]chan *sarama.ConsumerMessage
	topicErrors map[string]chan *sarama.ConsumerError

	logger Logger
	config *Config
}

// NewConsumerGroup creates a new consumer group instance using the given
// group storage and config.
func NewConsumerGroup(config *Config) (*ConsumerGroup, error) {
	if config == nil {
		return nil, errors.New("config can't be empty")
	}
	err := config.validate()
	if err != nil {
		return nil, fmt.Errorf("vaildate config failed, as %s", err)
	}

	cg := new(ConsumerGroup)
	cg.name = config.GroupID
	cg.topicList = config.TopicList
	cg.storage = newZKGroupStorage(config.ZkList, config.ZkSessionTimeout)
	brokerList, err := cg.storage.getBrokerList()
	if err != nil {
		return nil, fmt.Errorf("get brokerList err: %s", err.Error())
	}
	if len(brokerList) == 0 {
		return nil, errors.New("no broker alive")
	}

	if cg.saramaConsumer, err = sarama.NewConsumer(brokerList, config.SaramaConfig); err != nil {
		return nil, fmt.Errorf("sarama consumer initialize failed, because %s", err.Error())
	}

	cg.state = cgInit
	cg.id = genConsumerID()
	cg.stopper = make(chan struct{})
	cg.rebalanceTrigger = make(chan struct{})
	cg.rebalanceOnce = new(sync.Once)
	cg.stopOnce = new(sync.Once)
	cg.nextMessage = make(map[string]chan *sarama.ConsumerMessage)
	cg.topicErrors = make(map[string]chan *sarama.ConsumerError)
	for _, topic := range cg.topicList {
		cg.nextMessage[topic] = make(chan *sarama.ConsumerMessage)
		cg.topicErrors[topic] = make(chan *sarama.ConsumerError, config.ErrorChannelBufferSize)
	}
	cg.logger = newDefaultLogger(infoLevel)
	cg.config = config
	return cg, nil
}

// SetLogger sets the logger and you need to implement the Logger interface first.
func (cg *ConsumerGroup) SetLogger(logger Logger) {
	cg.logger = logger
}

// JoinGroup registers a consumer to the consumer group and starts to
// process messages from the topic list.
func (cg *ConsumerGroup) JoinGroup() error {

	// the program exits if the consumer fails to register
	err := cg.storage.registerConsumer(cg.name, cg.id, nil)
	if err != nil && err != zk.ErrNodeExists {
		return err
	}
	go cg.consumeTopicList()
	return nil
}

// ExitGroup close cg.stopper to notify consumer group stop consuming topic
// list.
func (cg *ConsumerGroup) ExitGroup() {
	cg.stopOnce.Do(func() { close(cg.stopper) })
}

// IsStopped returns true or false means if consumer group is stopped or not.
func (cg *ConsumerGroup) IsStopped() bool {
	return cg.state == cgStopped
}

func (cg *ConsumerGroup) triggerRebalance() {
	close(cg.rebalanceTrigger)
}

func (cg *ConsumerGroup) callRecover() {
	if err := recover(); err != nil {
		cg.logger.Errorf("[go-consumergroup] [%s] %s %s", cg.name, err, string(debug.Stack()))
		cg.ExitGroup()
	}
}

func (cg *ConsumerGroup) consumeTopicList() {
	var wg sync.WaitGroup

	defer func() {
		cg.state = cgStopped
		for _, topic := range cg.topicList {
			close(cg.nextMessage[topic])
			close(cg.topicErrors[topic])
		}
		err := cg.storage.deleteConsumer(cg.name, cg.id)
		if err != nil {
			cg.logger.Errorf("[go-consumergroup] [%s] Failed to delete consumer from zookeeper, err %s", cg.name, err.Error())
		}
	}()

	defer cg.callRecover()

CONSUME_TOPIC_LOOP:
	for {
		cg.logger.Infof("[go-consumergroup] [%s] consumer started", cg.name)
		cg.rebalanceOnce = new(sync.Once)
		cg.stopOnce = new(sync.Once)

		err := cg.checkRebalance()
		if err != nil {
			cg.logger.Errorf("[go-consumergroup] [%s] Failed to watch rebalance, err %s", cg.name, err.Error())
			cg.ExitGroup()
			return
		}

		wg.Add(1)
		go func() {
			defer cg.callRecover()
			defer wg.Done()
			cg.autoReconnect(cg.storage.(*zkGroupStorage).sessionTimeout / 3)
		}()

		for _, topic := range cg.topicList {
			wg.Add(1)
			go func(topic string) {
				defer cg.callRecover()
				defer wg.Done()
				cg.consumeTopic(topic)
			}(topic)
		}

		cg.state = cgStart

		// waiting for restart or rebalance
		select {
		case <-cg.rebalanceTrigger:
			cg.logger.Infof("[go-consumergroup] [%s] Trigger rebalance", cg.name)
			cg.ExitGroup()
			// stopper will be closed to notify partition consumers to
			// stop consuming when rebalance is triggered, and rebalanceTrigger
			// will also be closed to restart the consume topic loop in the meantime.
			wg.Wait()
			cg.stopper = make(chan struct{})
			cg.rebalanceTrigger = make(chan struct{})
			continue CONSUME_TOPIC_LOOP
		case <-cg.stopper: // triggered when ExitGroup() is called
			cg.logger.Infof("[go-consumergroup] [%s] ConsumeGroup is stopping", cg.name)
			wg.Wait()
			cg.logger.Infof("[go-consumergroup] [%s] ConsumerGroup was stopped", cg.name)
			return
		}
	}
}

func (cg *ConsumerGroup) consumeTopic(topic string) {
	var wg sync.WaitGroup
	cg.logger.Infof("[go-consumergroup] [%s, %s] Start to consume", cg.name, topic)

	defer func() {
		cg.logger.Infof("[go-consumergroup] [%s, %s] Stop to consume", cg.name, topic)
	}()

	partitions, err := cg.assignPartitionToConsumer(topic)
	if err != nil {
		cg.logger.Errorf("[go-consumergroup] [%s, %s] Failed to assign partitins, err %s", cg.name, topic, err.Error())
		return
	}
	cg.logger.Infof("[go-consumergroup] [%s, %s] partitions %v are assigned to this consumer", cg.name, topic, partitions)

	for _, partition := range partitions {
		wg.Add(1)
		cg.logger.Infof("[go-consumergroup] [%s, %s, %d] Start to consume partition", cg.name, topic, partition)
		go func(topic string, partition int32) {
			defer cg.callRecover()
			defer wg.Done()
			cg.consumePartition(topic, partition)
		}(topic, partition)
	}

	wg.Wait()
}

func (cg *ConsumerGroup) getPartitionConsumer(topic string, partition int32, nextOffset int64) (sarama.PartitionConsumer, error) {
	consumer, err := cg.saramaConsumer.ConsumePartition(topic, partition, nextOffset)
	if err == sarama.ErrOffsetOutOfRange {
		nextOffset = cg.config.OffsetAutoReset
		consumer, err = cg.saramaConsumer.ConsumePartition(topic, partition, nextOffset)
	}
	if err != nil {
		return nil, err
	}
	return consumer, nil
}

// GetTopicNextMessageChannel returns a unbuffered channel from which to get
// messages of a specified topic.
func (cg *ConsumerGroup) GetTopicNextMessageChannel(topic string) (<-chan *sarama.ConsumerMessage, error) {
	if cg.nextMessage[topic] == nil {
		return nil, errors.New("topic was not found")
	}
	return cg.nextMessage[topic], nil
}

// GetTopicErrorsChannel returns a buffered channel from which to get error
// messages of a specified topic.
func (cg *ConsumerGroup) GetTopicErrorsChannel(topic string) (<-chan *sarama.ConsumerError, error) {
	if cg.topicErrors[topic] == nil {
		return nil, errors.New("topic was not found")
	}
	return cg.topicErrors[topic], nil
}

func (cg *ConsumerGroup) consumePartition(topic string, partition int32) {
	var err error
	var consumer sarama.PartitionConsumer
	var mutex sync.Mutex
	var wg sync.WaitGroup

	select {
	case <-cg.stopper:
		return
	default:
	}

	defer func() {
		owner, err := cg.storage.getPartitionOwner(cg.name, topic, partition)
		if err != nil {
			cg.logger.Warnf("[go-consumergroup] [%s, %s, %d] Failed to get partition owner, err %s", cg.name, topic, partition, err.Error())
		}
		if cg.id == owner {
			err := cg.storage.releasePartition(cg.name, topic, partition)
			if err != nil {
				cg.logger.Warnf("[go-consumergroup] [%s, %s, %d] Failed to release partition, err %s", cg.name, topic, partition, err.Error())
			}
		}
	}()

	for i := 0; i < cg.config.ClaimPartitionRetry; i++ {
		if err = cg.storage.claimPartition(cg.name, topic, partition, cg.id); err != nil {
			cg.logger.Warnf("[go-consumergroup] [%s, %s, %d] Failed to claim partition, err %s", cg.name, topic, partition, err.Error())
		}
		if err == nil {
			cg.logger.Infof("[go-consumergroup] [%s, %s, %d] Claim partition success", cg.name, topic, partition)
			break
		}
		time.Sleep(cg.config.ClaimPartitionRetryInterval)
	}
	if err != nil {
		cg.logger.Errorf("[go-consumergroup] [%s, %s, %d] Failed to claim partition after %d retries", cg.name, topic, partition, cg.config.ClaimPartitionRetry)
		cg.ExitGroup()
		return
	}

	nextOffset, err := cg.storage.getOffset(cg.name, topic, partition)
	if err != nil {
		cg.logger.Errorf("[go-consumergroup] [%s, %s, %d] Failed to get offset, err %s", cg.name, topic, partition, err.Error())
		cg.ExitGroup()
		return
	}
	if nextOffset == -1 {
		nextOffset = cg.config.OffsetAutoReset
	}
	cg.logger.Debugf("[go-consumergroup] [%s, %s, %d] Get offset %d from offset storage", cg.name, topic, partition, nextOffset)

	consumer, err = cg.getPartitionConsumer(topic, partition, nextOffset)
	if err != nil {
		cg.logger.Errorf("[go-consumergroup] [%s, %s, %d] Failed to get partition consumer, err %s", cg.name, topic, partition, err.Error())
		cg.ExitGroup()
		return
	}
	cg.logger.Infof("[go-consumergroup] [%s, %s, %d] Consumer has started", cg.name, topic, partition)
	defer consumer.Close()

	prevCommitOffset := nextOffset

	if cg.config.OffsetAutoCommitEnable {
		wg.Add(1)
		go func() {
			defer cg.callRecover()
			defer wg.Done()
			timer := time.NewTimer(cg.config.OffsetAutoCommitInterval)
			for {
				select {
				case <-cg.stopper:
					return
				case <-timer.C:
					timer.Reset(cg.config.OffsetAutoCommitInterval)
					mutex.Lock()
					offset := nextOffset
					mutex.Unlock()

					if offset == prevCommitOffset {
						break
					}
					err := cg.storage.commitOffset(cg.name, topic, partition, offset)
					if err != nil {
						cg.logger.Warnf("[go-consumergroup] [%s, %s, %d] Failed to commit offset, err %s", cg.name, topic, partition, err.Error())
					} else {
						cg.logger.Debugf("[go-consumergroup] [%s, %s, %d] Commit offset %d to storage", cg.name, topic, partition, offset)
						prevCommitOffset = offset
					}
				}
			}
		}()
	}

	nextMessage := cg.nextMessage[topic]
	errors := cg.topicErrors[topic]

CONSUME_PARTITION_LOOP:
	for {
		select {
		case <-cg.stopper:
			break CONSUME_PARTITION_LOOP

		case error1 := <-consumer.Errors():
			errors <- error1
			//sends error messages to the error channel

		case message := <-consumer.Messages():
			select {
			case nextMessage <- message:
				mutex.Lock()
				nextOffset = message.Offset + 1
				mutex.Unlock()
			case <-cg.stopper:
				break CONSUME_PARTITION_LOOP
			}
		}
	}

	wg.Wait()

	if cg.config.OffsetAutoCommitEnable {
		if nextOffset != prevCommitOffset {
			err = cg.storage.commitOffset(cg.name, topic, partition, nextOffset)
			if err != nil {
				cg.logger.Errorf("[go-consumergroup] [%s, %s, %d] Failed to Commit offset %d, err ", cg.name, topic, partition, nextOffset, err.Error())
			} else {
				cg.logger.Debugf("[go-consumergroup] [%s, %s, %d] Commit offset %d to storage", cg.name, topic, partition, nextOffset)
			}
		}
	}

	cg.logger.Infof("[go-consumergroup] [%s, %s, %d] Consumer was stopped", cg.name, topic, partition)
}

func (cg *ConsumerGroup) getPartitionNum(topic string) (int, error) {
	partitions, err := cg.saramaConsumer.Partitions(topic)
	if err != nil {
		return 0, err
	}
	return len(partitions), nil
}

func (cg *ConsumerGroup) autoReconnect(interval time.Duration) {
	timer := time.NewTimer(interval)
	cg.logger.Infof("[go-consumergroup] [%s] The auto reconnect consumer goroutine was started", cg.name)
	defer cg.logger.Infof("[go-consumergroup] [%s] The auto reconnect consumer goroutine was stopped", cg.name)
	for {
		select {
		case <-cg.stopper:
			return
		case <-timer.C:
			timer.Reset(interval)
			exist, err := cg.storage.existsConsumer(cg.name, cg.id)
			if err != nil {
				cg.logger.Errorf("[go-consumergroup] [%s] Failed to check consumer exist, err %s", cg.name, err.Error())
				break
			}
			if exist {
				break
			}
			err = cg.storage.registerConsumer(cg.name, cg.id, nil)
			if err != nil {
				cg.logger.Errorf("[go-consumergroup] [%s] Faild to re-register consumer, err %s", cg.name, err.Error())
			}
		}
	}
}

func (cg *ConsumerGroup) checkRebalance() error {
	consumerListChange, err := cg.storage.watchConsumerList(cg.name)
	if err != nil {
		return err
	}

	go func() {
		defer cg.callRecover()
		cg.logger.Infof("[go-consumergroup] [%s] Rebalance checker was started", cg.name)
		select {
		case <-consumerListChange:
			cg.logger.Infof("[go-consumergroup] [%s] Trigger rebalance while consumers was changed", cg.name)
			cg.rebalanceOnce.Do(cg.triggerRebalance)
		case <-cg.stopper:
		}
		cg.logger.Infof("[go-consumergroup] [%s] Rebalance checker was exited", cg.name)
	}()

	return nil
}

func (cg *ConsumerGroup) assignPartitionToConsumer(topic string) ([]int32, error) {
	j := 0
	partNum, err := cg.getPartitionNum(topic)
	if err != nil || partNum == 0 {
		return nil, err
	}
	consumerList, err := cg.storage.getConsumerList(cg.name)
	if err != nil {
		return nil, err
	}
	consumerNum := len(consumerList)
	if consumerNum == 0 {
		return nil, errors.New("no consumer was found")
	}
	partitions := make([]int32, 0, partNum/consumerNum+1)
	for i := 0; i < partNum; i++ {
		id := consumerList[j%consumerNum]
		if id == cg.id {
			partitions = append(partitions, int32(i))
		}
		j++
	}
	return partitions, nil
}

func (cg *ConsumerGroup) CommitOffset(topic string, partition int32, offset int64) error {
	return cg.storage.commitOffset(cg.name, topic, partition, offset)
}
