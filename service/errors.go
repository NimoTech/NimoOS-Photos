package service

import "errors"

var (
	ErrNotFound        = errors.New("not found")
	ErrAlbumNameExists = errors.New("album name already exists")
	ErrInvalidInput    = errors.New("invalid input")
	ErrCoverNotInAlbum = errors.New("cover asset not in album")
)
