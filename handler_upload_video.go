package main

import (
	"context"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"

	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
)

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {
	maxMemory := 1 << 30
	r.Body = http.MaxBytesReader(w, r.Body, int64(maxMemory))

	videoIDString := r.PathValue("videoID")
	videoID, err := uuid.Parse(videoIDString)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid ID", err)
		return
	}

	token, err := auth.GetBearerToken(r.Header)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't find JWT", err)
		return
	}

	userID, err := auth.ValidateJWT(token, cfg.jwtSecret)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't validate JWT", err)
		return
	}

	video, err := cfg.db.GetVideo(videoID)
	if err != nil {
		respondWithError(
			w,
			http.StatusInternalServerError,
			"Not authorized to update this video",
			err,
		)
	}

	if video.UserID != userID {
		respondWithError(w, http.StatusUnauthorized, "Not authorized to update this video", nil)
	}

	file, header, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Unable to parse form file", err)
		return
	}
	defer file.Close()

	mediaType, _, err := mime.ParseMediaType(header.Header.Get("Content-Type"))
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid Content-Type", err)
		return
	}
	if mediaType != "video/mp4" {
		respondWithError(
			w,
			http.StatusBadRequest,
			"Invalid file type",
			nil,
		)
		return
	}

	dst, err := os.CreateTemp("", "tubely-upload.mp4")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error to create file on server", err)
	}
	defer os.Remove(dst.Name())
	defer dst.Close()

	if _, err = io.Copy(dst, file); err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error saving file", err)
	}

	_, err = dst.Seek(0, io.SeekStart)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Could not reset file pointer", err)
		return
	}

	filePath, err := filepath.Abs(dst.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Could not find file", err)
		return
	}

	aspectRatio, err := getVideoAspectRatio(filePath)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Could not get aspect ratios", err)
		return
	}

	prefix := aspectRatio
	if aspectRatio == "16:9" {
		prefix = "landscape"
	} else if aspectRatio == "9:16" {
		prefix = "portrait"
	}

	newFilePath, err := processVideoForFastStart(dst.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Could not get processed video", err)
		return
	}
	defer os.Remove(newFilePath)

	dat, err := os.Open(newFilePath)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Could not get processed video", err)
		return
	}
	defer dat.Close()

	key := prefix + "/" + getAssetPath(mediaType)
	_, err = cfg.s3Client.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket:      aws.String(cfg.s3Bucket),
		Key:         aws.String(key),
		Body:        dat,
		ContentType: aws.String(mediaType),
	})
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error uploading file to S3", err)
		return
	}

	val := cfg.s3Bucket + "," + key
	video.VideoURL = &val
	err = cfg.db.UpdateVideo(video)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Unable to update video data", err)
		return
	}

	video, err = cfg.dbVideoToSignedVideo(video)
	if err != nil {
		respondWithError(
			w,
			http.StatusInternalServerError,
			"Unable to generate video presigned link",
			err,
		)
		return
	}

	respondWithJSON(w, http.StatusOK, video)
}

func generatePresignedURL(
	s3Client *s3.Client,
	bucket, key string,
	expireTime time.Duration,
) (string, error) {
	client := s3.NewPresignClient(s3Client)
	req, err := client.PresignGetObject(context.Background(), &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	}, s3.WithPresignExpires(expireTime))
	if err != nil {
		return "", err
	}

	return req.URL, nil
}
