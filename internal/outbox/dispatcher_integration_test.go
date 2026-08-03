package outbox

import (
	"context"
	"os"
	"testing"
	"time"

	"readergo/internal/db"
	"readergo/internal/model"
	"readergo/internal/queue"

	"github.com/google/uuid"
	amqp "github.com/rabbitmq/amqp091-go"
	"gorm.io/gorm"
)

func TestDispatcherPublishesPersistentRoutableMessage(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	rabbitURL := os.Getenv("TEST_RABBITMQ_URL")
	if databaseURL == "" || rabbitURL == "" {
		t.Skip("set TEST_DATABASE_URL and TEST_RABBITMQ_URL to run integration test")
	}
	database, err := db.Open(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	user := model.User{Email: uuid.NewString() + "@example.test", PasswordHash: "integration", DisplayName: "integration"}
	if err := database.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// 删除临时视频会级联清理 job/outbox，再删除租户，避免污染共享集成测试库。
		_ = database.Where("tenant_id = ?", user.ID).Delete(&model.Video{}).Error
		_ = database.Delete(&user).Error
	})
	video := model.Video{TenantID: user.ID, SourceURL: "https://example.test/video.mp4", Status: model.VideoStatusUploading, UploadMethod: queue.UploadMethodURL}
	job := model.ProcessingJob{TenantID: user.ID, TaskType: model.TaskPrepare, Attempt: 1, Status: model.JobStatusPending}
	writer := NewWriter("readergo.video.jobs", "readergo.video.dlx")
	if err := database.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&video).Error; err != nil {
			return err
		}
		job.VideoID = video.ID
		if err := tx.Create(&job).Error; err != nil {
			return err
		}
		return writer.EnqueueJob(tx, job, queue.UploadMethodURL, time.Now().UTC())
	}); err != nil {
		t.Fatal(err)
	}

	topology := queue.TopologyConfig{JobsExchange: "readergo.video.jobs", DLXExchange: "readergo.video.dlx", ConsumerTimeout: 6 * time.Hour}
	publisher, err := queue.NewRabbitPublisher(rabbitURL, topology)
	if err != nil {
		t.Fatal(err)
	}
	defer publisher.Close()
	published, err := NewDispatcher(database, publisher, 10*time.Millisecond, 10).DispatchOnce(context.Background())
	if err != nil || published != 1 {
		t.Fatalf("DispatchOnce() = %d, %v; want one published message", published, err)
	}

	connection, err := amqp.Dial(rabbitURL)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	channel, err := connection.Channel()
	if err != nil {
		t.Fatal(err)
	}
	defer channel.Close()
	delivery, ok, err := channel.Get("readergo.video.prepare", false)
	if err != nil || !ok {
		t.Fatalf("get published message: ok=%v err=%v", ok, err)
	}
	if delivery.DeliveryMode != amqp.Persistent || delivery.MessageId == "" {
		t.Fatalf("message is not persistent or has no Outbox MessageId: %#v", delivery)
	}
	if err := delivery.Ack(false); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = publisher.Publish(ctx, queue.PublishMessage{
		MessageID: uuid.NewString(), Exchange: topology.JobsExchange, RoutingKey: "video.unbound", Body: []byte(`{"invalid":true}`),
	})
	if err == nil {
		t.Fatal("mandatory publish to an unbound routing key should fail")
	}
}
