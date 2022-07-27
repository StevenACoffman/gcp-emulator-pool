package gcpapi

import (
	"bytes"
	"context"
	"io"
	"path/filepath"
	"time"

	"cloud.google.com/go/storage"
	"google.golang.org/api/option"

	"github.com/Khan/districts-jobs/pkg/errors"
)

func NewCloudStorageClient(
	ctx context.Context,
	credentials []byte,
) (*storage.Client, error) {
	var gcsClient *storage.Client
	var cErr error
	if len(credentials) > 0 {
		gcsClient, cErr = storage.NewClient(
			ctx,
			option.WithCredentialsJSON(credentials),
		)
	} else {
		gcsClient, cErr = storage.NewClient(ctx)
	}
	return gcsClient, errors.Wrap(cErr, "Unable to get New Cloud Storage client")
}

// UploadFile uploads an object given the name and bytes.
func UploadFile(
	ctx context.Context,
	gcsClient *storage.Client,
	bucket,
	objectName string,
	fileBytes []byte,
	modTime time.Time,
) error {
	ctx, cancel := context.WithTimeout(ctx, time.Second*180)
	defer cancel()

	o := gcsClient.Bucket(bucket).Object(objectName)

	// Upload an object with storage.Writer.
	wc := o.NewWriter(ctx)
	if _, err := io.Copy(wc, bytes.NewBuffer(fileBytes)); err != nil {
		return errors.Newf("io.Copy: %w", err)
	}

	if err := wc.Close(); err != nil {
		return errors.Wrapf(err, "Unable to Close storage Writer for objectName %v", objectName)
	}

	_, err := o.Update(ctx, storage.ObjectAttrsToUpdate{
		ContentType:        "text/csv; charset=utf-8",
		ContentDisposition: "attachment;filename=" + filepath.Base(objectName),
		// we need to preserve the modTime as a CustomTime attribute to enable the DataTeam
		// KhanFlow pipeline to determine if the files have changed.
		CustomTime: modTime,
	})
	if err != nil {
		return errors.Wrapf(
			err,
			"Unable to Update ObjectAttrsToUpdate for objectName %v",
			objectName,
		)
	}

	return nil
}

// UploadCSVFile uploads an object given the name and bytes.
func UploadCSVFile(
	ctx context.Context,
	gcsClient *storage.Client,
	bucket,
	objectName string,
	fileBytes []byte,
	modTime time.Time,
) error {
	ctx, cancel := context.WithTimeout(ctx, time.Second*180)
	defer cancel()

	o := gcsClient.Bucket(bucket).Object(objectName)

	// Upload an object with storage.Writer.
	wc := o.NewWriter(ctx)
	if _, err := io.Copy(wc, bytes.NewBuffer(fileBytes)); err != nil {
		return errors.Newf("io.Copy: %w", err)
	}

	if err := wc.Close(); err != nil {
		return errors.Wrapf(err, "Unable to Close storage Writer for objectName %v", objectName)
	}
	// we need to set the content type and content disposition so the file is downloaded properly.
	objectAttrsToUpdate := storage.ObjectAttrsToUpdate{
		ContentType:        "text/csv; ; charset=utf-8",
		ContentDisposition: "attachment;filename=" + filepath.Base(objectName),
	}
	if _, err := o.Update(ctx, objectAttrsToUpdate); err != nil {
		return errors.Wrapf(err, "ObjectHandle(%q).Update: %v", objectName)
	}
	return nil
}
