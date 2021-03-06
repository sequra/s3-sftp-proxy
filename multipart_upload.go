package main

import (
	"bytes"
	"context"
	"fmt"
	"sync"

	"github.com/moriyoshi/s3-sftp-proxy/util"
	"github.com/sirupsen/logrus"

	aws_s3 "github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/s3/s3iface"
	"github.com/prometheus/client_golang/prometheus"
)

// S3PartUploadState state in which a part upload is
type S3PartUploadState int

const (
	// S3PartUploadStateAdding adding data to content
	S3PartUploadStateAdding = iota
	// S3PartUploadStateFull all data present, ready to be sent
	S3PartUploadStateFull
	// S3PartUploadStateSent already sent to S3
	S3PartUploadStateSent
	// S3PartUploadErrorSending error sending part to S3
	S3PartUploadErrorSending
	// S3PartUploadCancelled cancelled due to previous error
	S3PartUploadCancelled
)

// S3PartToUpload S3 part to be uploaded
type S3PartToUpload struct {
	// Part content
	content []byte
	// Part number (starting from 1)
	partNumber int64
	// Offset ranges already filled
	o *util.OffsetRanges
	// S3MultipartUploadWriter that contains this part
	uw *S3MultipartUploadWriter
	// Mutex to avoid problems accessing to the same part upload
	mtx sync.Mutex
	// State to know how to treat this part
	state S3PartUploadState
}

func (part *S3PartToUpload) getContent() ([]byte, error) {
	end := part.o.GetMaxValidOffset()
	if end == -1 {
		return nil, fmt.Errorf("Trying to obtain content of incomplete part %d", part.partNumber)
	}
	return part.content[0:end], nil
}

func (part *S3PartToUpload) copy(buf []byte, start int64, end int64) {
	copy(part.content[start:end], buf)
	part.o.Add(start, end)
}

func (part *S3PartToUpload) isFull() bool {
	return part.o.IsFull()
}

// S3MultipartUploadWriter uploads multiple parts to S3 having a writer interface
type S3MultipartUploadWriter struct {
	Ctx                    context.Context
	Bucket                 string
	Key                    Path
	S3                     s3iface.S3API
	ServerSideEncryption   *ServerSideEncryptionConfig
	Log                    logrus.FieldLogger
	MaxObjectSize          int64
	UploadMemoryBufferPool *MemoryBufferPool
	Info                   *PhantomObjectInfo
	PhantomObjectMap       *PhantomObjectMap
	RequestMethod          string
	mtx                    sync.Mutex
	completedParts         []*aws_s3.CompletedPart
	parts                  []*S3PartToUpload
	multiPartUploadID      *string
	err                    error
	uploadGroup            sync.WaitGroup
	UploadChan             chan<- *S3PartToUpload
}

// TransferError receives notifications when a transfer error is raised
// This notification is only present on `master` branch (2020-01-03), but
// not on current dependency (v0.10.1)
func (u *S3MultipartUploadWriter) TransferError(err error) {
	u.Log.WithField("exception", err).Debug("Transfer error")
}

// Close closes multipart upload writer
func (u *S3MultipartUploadWriter) Close() error {
	u.Log.Debug("S3MultipartUploadWriter.Close")

	u.PhantomObjectMap.RemoveByInfoPtr(u.Info)

	u.mtx.Lock()
	defer u.mtx.Unlock()

	err := u.err
	if err == nil {
		// Only one part -> use PutObject
		if len(u.parts) == 1 && u.multiPartUploadID == nil {
			part := u.parts[0]

			var content []byte
			content, err = part.getContent()
			if err == nil {
				err = u.s3PutObject(content)
				u.UploadMemoryBufferPool.Put(part.content)

				if err == nil {
					part.state = S3PartUploadStateSent
				} else {
					part.state = S3PartUploadErrorSending
				}
			} else {
				u.UploadMemoryBufferPool.Put(part.content)
				part.state = S3PartUploadErrorSending
			}
		} else {
			// More than 1 part -> MultiPartUpload used before, we have to send latest part, wait until all parts will be uploaded and then complete the job
			u.mtx.Unlock()

			err = u.enqueueUpload(u.parts[len(u.parts)-1])
			u.uploadGroup.Wait()

			u.mtx.Lock()
			if err == nil {
				pending := u.closePartsInStateAdding()
				if pending > 0 {
					err = fmt.Errorf("Closing upload and having %d pending parts to fill", pending)
				} else {
					err = u.err
					if err == nil {
						err = u.s3CompleteMultipartUpload()
					}
				}
			}
		}
	}

	if err != nil {
		u.Log.WithField("exception", err).Debug("Error closing upload")
		u.s3AbortMultipartUpload()
		u.closePartsInStateAdding()
		mOperationStatus.With(prometheus.Labels{"method": u.RequestMethod, "status": "failure"}).Inc()
	} else {
		mOperationStatus.With(prometheus.Labels{"method": u.RequestMethod, "status": "success"}).Inc()
	}
	return err
}

// WriteAt stores on memory the data sent to be uploaded and uploads it when a part
// is completed
func (u *S3MultipartUploadWriter) WriteAt(buf []byte, off int64) (int, error) {
	pending := int64(len(buf))
	offFinal := off + pending
	partSize := int64(u.UploadMemoryBufferPool.BufSize)
	partNumberInitial := int(off / partSize)
	partOffsetInitial := off % partSize
	bufOffset := int64(0)

	var err error
	u.mtx.Lock()
	err = u.err
	if err == nil && u.MaxObjectSize >= 0 && offFinal > u.MaxObjectSize {
		err = fmt.Errorf("file too large: maximum allowed size is %d bytes", u.MaxObjectSize)
	}

	if err != nil {
		u.Log.WithField("exception", err).Error("Error on WriteAt")
		u.s3AbortMultipartUpload()
		u.closePartsInStateAdding()
		u.err = err
		u.mtx.Unlock()
		mOperationStatus.With(prometheus.Labels{"method": u.RequestMethod, "status": "failure"}).Inc()
		return 0, err
	}

	partNumberFinal := int((off + pending - 1) / partSize)
	u.Log.Debugf("WriteAt len(buf)=%d, off=%d, partNumberInitial=%d, partOffsetInitial=%d", len(buf), off, partNumberInitial, partOffsetInitial)
	u.Info.SetSizeIfGreater(offFinal)
	if len(u.parts) <= partNumberFinal {
		newParts := make([]*S3PartToUpload, partNumberFinal+1)
		copy(newParts, u.parts)
		u.parts = newParts
	}
	u.mtx.Unlock()

	partNumber := partNumberInitial
	partOffset := partOffsetInitial
	for pending > 0 {
		u.mtx.Lock()
		part := u.parts[partNumber]
		if part == nil {
			u.Log.Debug("Getting memory buffer from pool")
			buf, err := u.UploadMemoryBufferPool.Get()
			if err != nil {
				u.Log.WithField("exception", err).Error("Error getting a memory buffer from pool")
				u.s3AbortMultipartUpload()
				u.closePartsInStateAdding()
				u.err = err
				u.mtx.Unlock()
				mOperationStatus.With(prometheus.Labels{"method": u.RequestMethod, "status": "failure"}).Inc()
				return 0, err
			}

			part = &S3PartToUpload{
				content:    buf,
				o:          util.NewOffsetRanges(partSize),
				uw:         u,
				state:      S3PartUploadStateAdding,
				partNumber: int64(partNumber + 1),
			}
			u.parts[partNumber] = part
		}
		u.mtx.Unlock()

		partOffsetFinal := partOffset + pending
		if partOffsetFinal > partSize {
			partOffsetFinal = partSize
		}
		partCopied := partOffsetFinal - partOffset

		part.mtx.Lock()
		if part.state < S3PartUploadStateFull {
			part.copy(buf[bufOffset:bufOffset+partCopied], partOffset, partOffsetFinal)
			if part.isFull() {
				err = u.enqueueUpload(part)
				if err != nil {
					part.mtx.Unlock()
					u.mtx.Lock()
					u.s3AbortMultipartUpload()
					u.closePartsInStateAdding()
					u.err = err
					u.mtx.Unlock()
					mOperationStatus.With(prometheus.Labels{"method": u.RequestMethod, "status": "failure"}).Inc()
					return 0, err
				}
			}
		} else {
			u.Log.WithField("partnumber", partNumber).Warn("Trying to add more data to an already full part")
		}
		part.mtx.Unlock()
		partNumber++
		pending -= partCopied
		bufOffset += partCopied
		partOffset = 0
	}
	mWritesBytesTotal.Add(float64(len(buf)))
	return len(buf), nil
}

func (u *S3MultipartUploadWriter) enqueueUpload(part *S3PartToUpload) error {
	if part.state < S3PartUploadStateFull {
		u.mtx.Lock()
		if u.multiPartUploadID == nil {
			if err := u.s3CreateMultipartUpload(); err != nil {
				u.mtx.Unlock()
				return err
			}
		}
		u.mtx.Unlock()

		log := u.Log.WithFields(logrus.Fields{
			"uploadid":   *u.multiPartUploadID,
			"partnumber": part.partNumber,
		})
		log.Debugf("Enqueuing part to be uploaded")
		part.state = S3PartUploadStateFull
		u.uploadGroup.Add(1)
		select {
		case <-u.Ctx.Done():
			log.Debug("Enqueue upload cancelled")
			return fmt.Errorf("Enqueue upload cancelled")
		case u.UploadChan <- part:
		}
	}
	return nil
}

func (u *S3MultipartUploadWriter) closePartsInStateAdding() int {
	pending := 0
	if u.parts != nil {
		for i := len(u.parts) - 1; i >= 0; i-- {
			part := u.parts[i]
			if part != nil {
				part.mtx.Lock()
				if part.state == S3PartUploadStateAdding {
					u.UploadMemoryBufferPool.Put(part.content)
					part.state = S3PartUploadCancelled
					pending++
				}
				part.mtx.Unlock()
			}
		}
	}
	return pending
}

// S3 related actions
func (u *S3MultipartUploadWriter) s3CreateMultipartUpload() error {
	key := u.Info.GetOne().Key.String()
	sse := u.ServerSideEncryption
	u.Log.Debugf("CreateMultipartUpload(sse=%v)", sse)

	params := &aws_s3.CreateMultipartUploadInput{
		ACL:                  &aclPrivate,
		Bucket:               &u.Bucket,
		Key:                  &key,
		ServerSideEncryption: sseTypes[sse.Type],
		SSECustomerAlgorithm: nilIfEmpty(sse.CustomerAlgorithm()),
		SSECustomerKey:       nilIfEmpty(sse.CustomerKey),
		SSECustomerKeyMD5:    nilIfEmpty(sse.CustomerKeyMD5),
		SSEKMSKeyId:          nilIfEmpty(sse.KMSKeyID),
	}

	resp, err := u.S3.CreateMultipartUploadWithContext(u.Ctx, params)
	if err != nil {
		u.Log.WithField("exception", err).Error("Error creating multipart upload")
		return err
	}
	u.Log.WithField("uploadid", *resp.UploadId).Debug("Multipart upload created correctly")
	u.multiPartUploadID = resp.UploadId
	return nil
}

func (u *S3MultipartUploadWriter) s3PutObject(content []byte) error {
	key := u.Info.GetOne().Key.String()
	sse := u.ServerSideEncryption
	u.Log.Debugf("PutObject(sse=%v)", sse)

	params := &aws_s3.PutObjectInput{
		ACL:                  &aclPrivate,
		Body:                 bytes.NewReader(content),
		Bucket:               &u.Bucket,
		Key:                  &key,
		ServerSideEncryption: sseTypes[sse.Type],
		SSECustomerAlgorithm: nilIfEmpty(sse.CustomerAlgorithm()),
		SSECustomerKey:       nilIfEmpty(sse.CustomerKey),
		SSECustomerKeyMD5:    nilIfEmpty(sse.CustomerKeyMD5),
		SSEKMSKeyId:          nilIfEmpty(sse.KMSKeyID),
	}
	if _, err := u.S3.PutObjectWithContext(u.Ctx, params); err != nil {
		u.Log.WithField("exception", err).Error("Error putting object")
		return err
	}
	return nil
}

func (u *S3MultipartUploadWriter) s3AbortMultipartUpload() error {
	if u.multiPartUploadID != nil {
		key := u.Info.GetOne().Key.String()
		sse := u.ServerSideEncryption
		log := u.Log.WithField("uploadid", *u.multiPartUploadID)
		log.Debugf("AbortMultipartUpload(sse=%v)", sse)

		params := &aws_s3.AbortMultipartUploadInput{
			Bucket:   &u.Bucket,
			Key:      &key,
			UploadId: u.multiPartUploadID,
		}
		u.multiPartUploadID = nil
		if _, err := u.S3.AbortMultipartUploadWithContext(u.Ctx, params); err != nil {
			log.WithField("exception", err).Error("Error aborting multipart upload")
			return err
		}
	}

	return nil
}

func (u *S3MultipartUploadWriter) s3CompleteMultipartUpload() error {
	key := u.Info.GetOne().Key.String()
	sse := u.ServerSideEncryption
	log := u.Log.WithField("uploadid", *u.multiPartUploadID)
	log.Debugf("CompleteMultipartUpload(sse=%v)", sse)

	params := &aws_s3.CompleteMultipartUploadInput{
		Bucket:          &u.Bucket,
		Key:             &key,
		UploadId:        u.multiPartUploadID,
		MultipartUpload: &aws_s3.CompletedMultipartUpload{Parts: u.completedParts},
	}
	if _, err := u.S3.CompleteMultipartUploadWithContext(u.Ctx, params); err != nil {
		log.WithField("exception", err).Error("Error completing multipart upload")
		return err
	}
	return nil
}

func (u *S3MultipartUploadWriter) s3UploadPart(part *S3PartToUpload) error {
	key := u.Info.GetOne().Key.String()
	sse := u.ServerSideEncryption
	log := u.Log.WithFields(logrus.Fields{
		"uploadid":   *u.multiPartUploadID,
		"partnumber": part.partNumber,
	})
	log.Debugf("UploadPart(sse=%v)", sse)

	var content []byte
	var err error

	content, err = part.getContent()
	if err != nil {
		return err
	}

	params := &aws_s3.UploadPartInput{
		Bucket:               &u.Bucket,
		Key:                  &key,
		Body:                 bytes.NewReader(content),
		UploadId:             u.multiPartUploadID,
		SSECustomerAlgorithm: nilIfEmpty(sse.CustomerAlgorithm()),
		SSECustomerKey:       nilIfEmpty(sse.CustomerKey),
		SSECustomerKeyMD5:    nilIfEmpty(sse.CustomerKeyMD5),
		PartNumber:           &part.partNumber,
	}

	resp, err := u.S3.UploadPartWithContext(u.Ctx, params)

	if err != nil {
		log.WithField("exception", err).Error("Error uploading part to S3")
		return err
	}

	if len(u.completedParts) < len(u.parts) {
		newCompletedParts := make([]*aws_s3.CompletedPart, len(u.parts))
		copy(newCompletedParts, u.completedParts)
		u.completedParts = newCompletedParts
	}
	completed := &aws_s3.CompletedPart{ETag: resp.ETag, PartNumber: &(part.partNumber)}
	u.completedParts[part.partNumber-1] = completed
	return nil
}

// seterr is a thread-safe setter for the error object
func (u *S3MultipartUploadWriter) seterr(e error) {
	u.mtx.Lock()
	defer u.mtx.Unlock()

	u.err = e
}

// S3UploadWorkers object to manage S3 upload workers
type S3UploadWorkers struct {
	ctx     context.Context
	workers int
	log     logrus.FieldLogger
	wg      sync.WaitGroup
}

// NewS3UploadWorkers creates new upload workers to take pending part uploads from a channel and
// upload them to S3
func NewS3UploadWorkers(ctx context.Context, workers int, log logrus.FieldLogger) *S3UploadWorkers {
	return &S3UploadWorkers{
		ctx:     ctx,
		workers: workers,
		log:     log,
	}
}

// Start starts workers
func (w *S3UploadWorkers) Start() chan<- *S3PartToUpload {
	uploadChan := make(chan *S3PartToUpload)

	w.wg.Add(w.workers)
	for c := 0; c < w.workers; c++ {
		go func(wn int) {
			defer w.wg.Done()

			log := w.log.WithField("worker", fmt.Sprintf("s3-upload-worker-%d", wn))
			log.Debug("Waiting for upload jobs")
			for {
				select {
				case <-w.ctx.Done():
					log.Debug("Worker ended")
					return
				case part, ok := <-uploadChan:
					if !ok {
						log.Debug("Upload channel closed")
						return
					}
					w.uploadPart(part)
				}
			}
		}(c)
	}

	return uploadChan
}

// WaitForCompletion waits until all workers finish their job
func (w *S3UploadWorkers) WaitForCompletion() {
	w.wg.Wait()
}

func (w *S3UploadWorkers) uploadPart(part *S3PartToUpload) {
	part.mtx.Lock()
	defer part.mtx.Unlock()

	u := part.uw
	defer u.uploadGroup.Done()

	if part.state != S3PartUploadStateFull {
		u.Log.WithFields(logrus.Fields{
			"uploadid":   *u.multiPartUploadID,
			"partnumber": part.partNumber,
		}).Warnf("Invalid state: %d", part.state)
		return
	}

	err := u.s3UploadPart(part)
	u.UploadMemoryBufferPool.Put(part.content)

	if err != nil {
		part.state = S3PartUploadErrorSending
		u.seterr(err)
	} else {
		part.state = S3PartUploadStateSent
	}
}
