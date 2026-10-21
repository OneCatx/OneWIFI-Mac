package uim

import (
	"context"
	"encoding/binary"
	"errors"
	"slices"

	"github.com/damonto/uicc-go/qcom"
	"github.com/damonto/uicc-go/qcom/tlv"
)

type RawFileAttributes struct {
	FileSize    uint16
	FileID      uint16
	FileType    QMIFileType
	RecordSize  uint16
	RecordCount uint16
	Raw         []byte
}

func (r *RawFileAttributes) UnmarshalBinary(data []byte) error {
	if len(data) < 9 {
		return errors.New("reading file attributes: attributes payload is truncated")
	}

	r.FileSize = binary.LittleEndian.Uint16(data[:2])
	r.FileID = binary.LittleEndian.Uint16(data[2:4])
	r.FileType = QMIFileType(data[4])
	r.RecordSize = binary.LittleEndian.Uint16(data[5:7])
	r.RecordCount = binary.LittleEndian.Uint16(data[7:9])

	if len(data) < 26 {
		return nil
	}

	rawLength := int(binary.LittleEndian.Uint16(data[24:26]))
	if len(data) < 26+rawLength {
		return errors.New("reading file attributes: raw data is truncated")
	}

	r.Raw = slices.Clone(data[26 : 26+rawLength])
	return nil
}

func (r *Reader) FileAttributes(ctx context.Context, file File) (FileAttributes, error) {
	return r.GetFileAttributes(ctx, file)
}

func (r *Reader) GetFileAttributes(ctx context.Context, file File) (FileAttributes, error) {
	response, err := r.fileAttributesResponse(ctx, file)
	if err != nil {
		return FileAttributes{}, err
	}
	return decodeReaderFileAttributes(response)
}

func (r *Reader) ReadTransparent(ctx context.Context, req TransparentRead) ([]byte, error) {
	length := req.Length
	if length == 0 {
		attrs, err := r.FileAttributes(ctx, req.File)
		if err != nil {
			return nil, err
		}
		if attrs.FileStructure != FileStructureTransparent {
			return nil, errors.New("reading transparent file: unexpected file structure")
		}
		if req.Offset > attrs.FileSize {
			return nil, errors.New("reading transparent file: offset exceeds file size")
		}
		length = attrs.FileSize - req.Offset
	}

	response, err := r.transparentResponse(ctx, req.File, req.Offset, length)
	if err != nil {
		return nil, err
	}

	value, ok := tlv.Value(response.TLVs, 0x11)
	if !ok {
		return nil, errors.New("reading transparent file: read result TLV missing")
	}
	return decodeLengthPrefixedBytes(value)
}

func (r *Reader) ReadRecord(ctx context.Context, req RecordRead) ([]byte, error) {
	if req.Record == 0 {
		return nil, errors.New("reading record file: record number is zero")
	}

	length := req.Length
	if length == 0 {
		attrs, err := r.FileAttributes(ctx, req.File)
		if err != nil {
			return nil, err
		}
		if attrs.FileStructure != FileStructureLinearFixed {
			return nil, errors.New("reading record file: unexpected file structure")
		}
		length = attrs.RecordSize
	}

	response, err := r.recordResponse(ctx, req.File, req.Record, length)
	if err != nil {
		return nil, err
	}

	value, ok := tlv.Value(response.TLVs, 0x11)
	if !ok {
		return nil, errors.New("reading record file: read result TLV missing")
	}
	return decodeLengthPrefixedBytes(value)
}

func (r *Reader) transparentResponse(
	ctx context.Context,
	file File,
	offset uint16,
	length uint16,
) (qcom.Response, error) {
	fileValue, err := putFileValue(file.Path)
	if err != nil {
		return qcom.Response{}, err
	}

	info := joinBytes(
		binary.LittleEndian.AppendUint16(nil, offset),
		binary.LittleEndian.AppendUint16(nil, length),
	)
	resp, err := r.request(ctx, qcom.MessageReadTransparent, tlv.TLVs{
		tlv.Bytes(0x01, putSessionValue(file.Session, file.AID)),
		tlv.Bytes(0x02, fileValue),
		tlv.Bytes(0x03, info),
	})
	if err != nil {
		return qcom.Response{}, err
	}
	if err := cardResultOK(resp); err != nil {
		return qcom.Response{}, err
	}
	return resp, nil
}

func (r *Reader) recordResponse(
	ctx context.Context,
	file File,
	record uint16,
	length uint16,
) (qcom.Response, error) {
	fileValue, err := putFileValue(file.Path)
	if err != nil {
		return qcom.Response{}, err
	}

	recordValue := joinBytes(
		binary.LittleEndian.AppendUint16(nil, record),
		binary.LittleEndian.AppendUint16(nil, length),
	)
	resp, err := r.request(ctx, qcom.MessageReadRecord, tlv.TLVs{
		tlv.Bytes(0x01, putSessionValue(file.Session, file.AID)),
		tlv.Bytes(0x02, fileValue),
		tlv.Bytes(0x03, recordValue),
	})
	if err != nil {
		return qcom.Response{}, err
	}
	if err := cardResultOK(resp); err != nil {
		return qcom.Response{}, err
	}
	return resp, nil
}

func (r *Reader) fileAttributesResponse(
	ctx context.Context,
	file File,
) (qcom.Response, error) {
	fileValue, err := putFileValue(file.Path)
	if err != nil {
		return qcom.Response{}, err
	}

	resp, err := r.request(ctx, qcom.MessageGetFileAttributes, tlv.TLVs{
		tlv.Bytes(0x01, putSessionValue(file.Session, file.AID)),
		tlv.Bytes(0x02, fileValue),
	})
	if err != nil {
		return qcom.Response{}, err
	}
	if err := cardResultOK(resp); err != nil {
		return qcom.Response{}, err
	}
	return resp, nil
}

func decodeReaderFileAttributes(resp qcom.Response) (FileAttributes, error) {
	value, ok := tlv.Value(resp.TLVs, 0x11)
	if !ok {
		return FileAttributes{}, errors.New("reading file attributes: attributes TLV missing")
	}

	var attrs RawFileAttributes
	if err := attrs.UnmarshalBinary(value); err != nil {
		return FileAttributes{}, err
	}

	return FileAttributes{
		FileSize:      attrs.FileSize,
		RecordSize:    attrs.RecordSize,
		RecordCount:   attrs.RecordCount,
		FileType:      fileTypeToSIMFileType(attrs.FileType),
		FileStructure: fileTypeToSIMFileStructure(attrs.FileType),
	}, nil
}

func fileTypeToSIMFileStructure(fileType QMIFileType) FileStructure {
	switch fileType {
	case QMIFileTypeTransparent:
		return FileStructureTransparent
	case QMIFileTypeLinearFixed:
		return FileStructureLinearFixed
	default:
		return 0
	}
}

func fileTypeToSIMFileType(fileType QMIFileType) FileType {
	switch fileType {
	case QMIFileTypeTransparent, QMIFileTypeCyclic, QMIFileTypeLinearFixed:
		return FileTypeWorkingEF
	case QMIFileTypeDedicated, QMIFileTypeMaster:
		return FileTypeDFOrADF
	default:
		return FileType(fileType)
	}
}
