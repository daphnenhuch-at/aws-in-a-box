package itest

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"aws-in-a-box/server"
	s3Impl "aws-in-a-box/services/s3"
)

var bucket = "test-bucket"

func makeClientServerPair() (*s3.Client, *http.Server) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	impl, err := s3Impl.New(s3Impl.Options{
		Addr: listener.Addr().String(),
	})
	if err != nil {
		panic(err)
	}
	srv := server.NewWithHandlerChain(s3Impl.NewHandler(slog.Default(), impl))
	go srv.Serve(listener)

	client := s3.New(s3.Options{
		EndpointResolver: s3.EndpointResolverFromURL("http://" + listener.Addr().String()),
		// Disable the subdomain addressing since it won't work (test-bucket.127.0.0.1)
		UsePathStyle: true,
		Retryer:      aws.NopRetryer{},
	})
	_, err = client.CreateBucket(context.Background(), &s3.CreateBucketInput{
		Bucket: &bucket,
	})
	if err != nil {
		panic(err)
	}

	return client, srv
}
func TestMultipartUpload(t *testing.T) {
	ctx := context.Background()
	client, srv := makeClientServerPair()
	defer srv.Shutdown(ctx)

	kmsKey := "custom-kms-key"
	key := "test-key"
	kmsContext := "foo=bar"
	upload, err := client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket:                  &bucket,
		Key:                     &key,
		ServerSideEncryption:    types.ServerSideEncryptionAwsKms,
		SSEKMSKeyId:             &kmsKey,
		SSEKMSEncryptionContext: &kmsContext,
	})
	if err != nil {
		t.Fatal(err)
	}
	id := upload.UploadId

	var parts []types.CompletedPart
	for i, s := range []string{"hello", " world"} {
		output, err := client.UploadPart(ctx, &s3.UploadPartInput{
			PartNumber: int32(i),
			Bucket:     &bucket,
			Key:        &key,
			UploadId:   id,
			Body:       strings.NewReader(s),
		})
		if output.ServerSideEncryption != types.ServerSideEncryptionAwsKms {
			t.Fatal("missing SSE header")
		}
		if *output.SSEKMSKeyId != kmsKey {
			t.Fatal("missing KMS key header: ", *output.SSEKMSKeyId)
		}
		if err != nil {
			t.Fatal(err)
		}
		parts = append(parts, types.CompletedPart{
			ETag:       output.ETag,
			PartNumber: int32(i),
		})
	}

	output, err := client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket:   &bucket,
		Key:      &key,
		UploadId: id,
		MultipartUpload: &types.CompletedMultipartUpload{
			Parts: parts,
		},
	})
	if output.ServerSideEncryption != types.ServerSideEncryptionAwsKms {
		t.Fatal("missing SSE header")
	}
	if *output.SSEKMSKeyId != kmsKey {
		t.Fatal("missing KMS key header")
	}
	if err != nil {
		t.Fatal(err)
	}

	object, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: &bucket,
		Key:    &key,
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(object.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello world" {
		t.Fatal("Unexpected data: ", string(data))
	}
	if object.ServerSideEncryption != types.ServerSideEncryptionAwsKms {
		t.Fatal("missing SSE header")
	}
	if *object.SSEKMSKeyId != kmsKey {
		t.Fatal("missing KMS key header")
	}

	partsOutput, err := client.ListParts(ctx, &s3.ListPartsInput{
		Bucket:   &bucket,
		Key:      &key,
		UploadId: id,
		MaxParts: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(partsOutput.Parts, []types.Part{{
		ETag:       parts[0].ETag,
		Size:       5,
		PartNumber: 0,
	}}) {
		t.Fatal("wrong parts", partsOutput.Parts)
	}
	if !partsOutput.IsTruncated {
		t.Fatal("not truncated")
	}
	partsOutput, err = client.ListParts(ctx, &s3.ListPartsInput{
		Bucket:           &bucket,
		Key:              &key,
		UploadId:         id,
		MaxParts:         1,
		PartNumberMarker: partsOutput.NextPartNumberMarker,
	})
	if err != nil {
		t.Fatal(err)
	}
	if partsOutput.IsTruncated {
		t.Fatal("truncated")
	}
	if !reflect.DeepEqual(partsOutput.Parts, []types.Part{{
		ETag:       parts[1].ETag,
		Size:       6,
		PartNumber: 1,
	}}) {
		t.Fatal("wrong parts", partsOutput.Parts)
	}
}

type RangeTestCase struct {
	Name  string
	Range string
	Body  string
}

func TestRangeQuery(t *testing.T) {
	ctx := context.Background()
	client, srv := makeClientServerPair()
	defer srv.Shutdown(ctx)

	key := "test-key"
	upload, err := client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: &bucket,
		Key:    &key,
	})
	if err != nil {
		t.Fatal(err)
	}
	id := upload.UploadId

	var parts []types.CompletedPart
	for i, s := range []string{"hello", " world ", "hi"} {
		output, err := client.UploadPart(ctx, &s3.UploadPartInput{
			PartNumber: int32(i),
			Bucket:     &bucket,
			Key:        &key,
			UploadId:   id,
			Body:       strings.NewReader(s),
		})
		if err != nil {
			t.Fatal(err)
		}
		parts = append(parts, types.CompletedPart{
			ETag:       output.ETag,
			PartNumber: int32(i),
		})
	}

	_, err = client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket:   &bucket,
		Key:      &key,
		UploadId: id,
		MultipartUpload: &types.CompletedMultipartUpload{
			Parts: parts,
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	testCases := []RangeTestCase{
		{Name: "entire range", Range: "bytes=0-13", Body: "hello world hi"},
		{Name: "Skip entire first part and half of second part", Range: "bytes=8-13", Body: "rld hi"},
		{Name: "Prefix", Range: "bytes=0-8", Body: "hello wor"},
		{Name: "Suffix", Range: "bytes=-4", Body: "d hi"},
	}

	for _, testCase := range testCases {
		t.Run(testCase.Name, func(t *testing.T) {
			// Range Query for whole thing.
			output, err := client.GetObject(ctx, &s3.GetObjectInput{
				Bucket: &bucket,
				Key:    &key,
				Range:  &testCase.Range,
			})
			if err != nil {
				t.Fatal(err)
			}
			data, err := io.ReadAll(output.Body)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(data, []byte(testCase.Body)) {
				t.Fatal("{} wrong body", string(data))
			}
		})

	}

}

func TestObjectTagging(t *testing.T) {
	ctx := context.Background()
	client, srv := makeClientServerPair()
	defer srv.Shutdown(ctx)

	key := "test-key"
	_, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:  &bucket,
		Key:     &key,
		Tagging: aws.String("key=value"),
		Body:    strings.NewReader("hello"),
	})
	if err != nil {
		t.Fatal(err)
	}

	tagging, err := client.GetObjectTagging(ctx, &s3.GetObjectTaggingInput{
		Bucket: &bucket,
		Key:    &key,
	})
	if err != nil {
		t.Fatal(err)
	}
	tags := tagging.TagSet
	if len(tags) != 1 {
		t.Fatal("bad tags", tagging.TagSet)
	}
	if *tags[0].Key != "key" {
		t.Fatal("bad tag")
	}
	if *tags[0].Value != "value" {
		t.Fatal("bad value")
	}

	_, err = client.PutObjectTagging(ctx, &s3.PutObjectTaggingInput{
		Bucket: &bucket,
		Key:    &key,
		Tagging: &types.Tagging{TagSet: []types.Tag{
			{
				Key:   aws.String("key"),
				Value: aws.String("value2"),
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	tagging, err = client.GetObjectTagging(ctx, &s3.GetObjectTaggingInput{
		Bucket: &bucket,
		Key:    &key,
	})
	if err != nil {
		t.Fatal(err)
	}
	tags = tagging.TagSet
	if len(tags) != 1 {
		t.Fatal("bad tags", tagging.TagSet)
	}
	if *tags[0].Key != "key" {
		t.Fatal("bad tag")
	}
	if *tags[0].Value != "value2" {
		t.Fatal("bad value", *tags[0].Value)
	}
}

func TestObjectTagging_NoTags(t *testing.T) {
	ctx := context.Background()
	client, srv := makeClientServerPair()
	defer srv.Shutdown(ctx)

	key := "test-key"
	_, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: &bucket,
		Key:    &key,
		Body:   strings.NewReader("hello"),
	})
	if err != nil {
		t.Fatal(err)
	}

	tagging, err := client.GetObjectTagging(ctx, &s3.GetObjectTaggingInput{
		Bucket: &bucket,
		Key:    &key,
	})
	if err != nil {
		t.Fatal(err)
	}
	tags := tagging.TagSet
	if len(tags) != 0 {
		t.Fatal("bad tags", tagging.TagSet)
	}
}

func TestBucketTagging(t *testing.T) {
	ctx := context.Background()
	client, srv := makeClientServerPair()
	defer srv.Shutdown(ctx)

	tagging, err := client.GetBucketTagging(ctx, &s3.GetBucketTaggingInput{
		Bucket: &bucket,
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(tagging.TagSet) != 0 {
		t.Fatal("bad tags")
	}

	tag := types.Tag{
		Key:   aws.String("key"),
		Value: aws.String("value"),
	}

	_, err = client.PutBucketTagging(ctx, &s3.PutBucketTaggingInput{
		Bucket: &bucket,
		Tagging: &types.Tagging{
			TagSet: []types.Tag{tag},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	tagging, err = client.GetBucketTagging(ctx, &s3.GetBucketTaggingInput{
		Bucket: &bucket,
	})
	if err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(tagging.TagSet, []types.Tag{tag}) {
		t.Fatal("bad tags")
	}

	_, err = client.DeleteBucketTagging(ctx, &s3.DeleteBucketTaggingInput{
		Bucket: &bucket,
	})
	if err != nil {
		t.Fatal(err)
	}

	tagging, err = client.GetBucketTagging(ctx, &s3.GetBucketTaggingInput{
		Bucket: &bucket,
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(tagging.TagSet) != 0 {
		t.Fatal("bad tags")
	}
}

func TestDeleteObjects(t *testing.T) {
	ctx := context.Background()
	client, srv := makeClientServerPair()
	defer srv.Shutdown(ctx)

	for i := 0; i < 4; i++ {
		_, err := client.PutObject(ctx, &s3.PutObjectInput{
			Bucket: &bucket,
			Key:    aws.String(strconv.Itoa(i)),
			Body:   strings.NewReader(""),
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	_, err := client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: &bucket,
		Key:    aws.String("1"),
	})
	if err != nil {
		t.Fatal(err)
	}

	output, err := client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
		Bucket: &bucket,
		Delete: &types.Delete{
			Objects: []types.ObjectIdentifier{
				{Key: aws.String("2")},
			},
			Quiet: true,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(output.Deleted) != 0 {
		t.Fatal("Not quiet!")
	}

	output, err = client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
		Bucket: &bucket,
		Delete: &types.Delete{
			Objects: []types.ObjectIdentifier{
				{Key: aws.String("3")},
				{Key: aws.String("4")},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(output.Deleted, []types.DeletedObject{
		{Key: aws.String("3")},
	}) {
		t.Fatal("wrong deletion?", output.Deleted)
	}
	if !reflect.DeepEqual(output.Errors, []types.Error{
		{Key: aws.String("4"), Code: aws.String("NotFound"), Message: aws.String("")},
	}) {
		t.Fatal("wrong error?", output.Errors)
	}
}

func TestListObjectsV2(t *testing.T) {
	ctx := context.Background()
	client, srv := makeClientServerPair()
	defer srv.Shutdown(ctx)

	for i := 0; i < 20; i++ {
		_, err := client.PutObject(ctx, &s3.PutObjectInput{
			Bucket: &bucket,
			Key:    aws.String(strconv.Itoa(i)),
			Body:   strings.NewReader(""),
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	resp, err := client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket: &bucket,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Default limit of 1000
	if len(resp.Contents) != 20 {
		t.Fatal("not 20 contents", resp.Contents)
	}
	// Contents should be sorted in string order
	prevKey := ""
	for _, content := range resp.Contents {
		if *content.Key <= prevKey {
			t.Fatal("not sorted output", prevKey, content.Key)
		}
		prevKey = *content.Key
	}

	// It should respect maxKeys and startAfter
	startAfter := "14"
	resp, err = client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket:     &bucket,
		MaxKeys:    2,
		StartAfter: &startAfter,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Contents) != 2 {
		t.Fatal("not 4 contents", resp.Contents)
	}
	// All of the contents should be in sorted order, and should be after "14"
	if *resp.Contents[0].Key != "14" {
		t.Fatal("should have found 14", resp.Contents[0])
	}
	if *resp.Contents[1].Key != "15" {
		t.Fatal("should have found 15", resp.Contents[1])
	}
	// It should give us a reasonable continuation token.
	if resp.NextContinuationToken == nil || *resp.NextContinuationToken != "16" {
		t.Fatal("continuation token should be 16", resp.ContinuationToken)
	}

	// It should respect Prefix
	prefix := "1"
	resp, err = client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket: &bucket,
		// We expect 11 keys, include an extra to verify behavior
		MaxKeys: 12,
		Prefix:  &prefix,
	})
	if len(resp.Contents) != 11 {
		t.Fatal("not 11 contents", resp.Contents)
	}
	for _, content := range resp.Contents {
		if !strings.HasPrefix(*content.Key, prefix) {
			t.Fatal("not has prefix ", prevKey, content.Key)
		}
	}
	if resp.ContinuationToken != nil {
		t.Fatal("expected nil continuation token", resp)
	}

}
