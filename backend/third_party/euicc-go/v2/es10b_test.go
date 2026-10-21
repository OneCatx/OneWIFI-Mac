package sgp22

import (
	"testing"

	"github.com/damonto/euicc-go/bertlv"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestListNotificationResponseErrorChoice(t *testing.T) {
	response := new(ListNotificationResponse)
	tlv := bertlv.NewChildren(
		bertlv.ContextSpecific.Constructed(40),
		bertlv.NewValue(bertlv.ContextSpecific.Primitive(1), []byte{0x7f}),
	)

	err := response.UnmarshalBERTLV(tlv)

	assert.ErrorIs(t, err, ErrUndefined)
}

func TestListNotificationRequestOmitsEmptyFilter(t *testing.T) {
	request, err := new(ListNotificationRequest).MarshalBERTLV()

	require.NoError(t, err)
	assert.Equal(t, []byte{0xbf, 0x28, 0x00}, request.Bytes())
}

func TestListNotificationRequestEncodesFilterBits(t *testing.T) {
	request, err := (&ListNotificationRequest{
		Filter: map[NotificationEvent]bool{
			NotificationEventInstall: true,
			NotificationEventDelete:  true,
		},
	}).MarshalBERTLV()

	require.NoError(t, err)
	assert.Equal(t, []byte{0xbf, 0x28, 0x04, 0x81, 0x02, 0x04, 0x90}, request.Bytes())
}

func TestRetrieveNotificationsListRequestEncodesSearchCriteria(t *testing.T) {
	criteria, err := bertlv.MarshalValue(bertlv.ContextSpecific.Primitive(0), SequenceNumber(1))
	require.NoError(t, err)

	request, err := (&RetrieveNotificationsListRequest{SearchCriteria: criteria}).MarshalBERTLV()

	require.NoError(t, err)
	assert.Equal(t, []byte{0xbf, 0x2b, 0x05, 0xa0, 0x03, 0x80, 0x01, 0x01}, request.Bytes())
}

func TestRetrieveNotificationsListRequestOmitsSearchCriteria(t *testing.T) {
	request, err := new(RetrieveNotificationsListRequest).MarshalBERTLV()

	require.NoError(t, err)
	assert.Equal(t, []byte{0xbf, 0x2b, 0x00}, request.Bytes())
}

func TestPrepareDownloadRequestNeedConfirmationCodeUsesBooleanTag(t *testing.T) {
	request := &PrepareDownloadRequest{
		Signed2: bertlv.NewChildren(
			bertlv.ContextSpecific.Constructed(0),
			bertlv.NewValue(bertlv.Universal.Primitive(1), []byte{0xff}),
		),
	}

	assert.True(t, request.NeedConfirmationCode())
}

func TestPrepareDownloadRequestEncodesHashCcAsOctetString(t *testing.T) {
	request := &PrepareDownloadRequest{
		TransactionID: []byte{0x01, 0x02},
		Signed2: bertlv.NewChildren(
			bertlv.ContextSpecific.Constructed(0),
			bertlv.NewValue(bertlv.ContextSpecific.Primitive(0), []byte{0x01, 0x02}),
			bertlv.NewValue(bertlv.Universal.Primitive(1), []byte{0xff}),
		),
		Signature2:       bertlv.NewValue(bertlv.Application.Primitive(55), []byte{0x03}),
		Certificate:      bertlv.NewChildren(bertlv.ContextSpecific.Constructed(3)),
		ConfirmationCode: []byte("1234"),
	}

	tlv, err := request.MarshalBERTLV()
	require.NoError(t, err)

	expected := []byte{
		0xbf, 0x21, 0x31,
		0xa0, 0x07, 0x80, 0x02, 0x01, 0x02, 0x01, 0x01, 0xff,
		0x5f, 0x37, 0x01, 0x03,
		0x04, 0x20,
	}
	expected = append(expected, request.HashedConfirmationCode()...)
	expected = append(expected, 0xa3, 0x00)
	assert.Equal(t, expected, tlv.Bytes())
}

func TestRetrieveNotificationsListResponseAllowsEmptyList(t *testing.T) {
	response := new(RetrieveNotificationsListResponse)
	tlv := bertlv.NewChildren(
		bertlv.ContextSpecific.Constructed(43),
		bertlv.NewChildren(bertlv.ContextSpecific.Constructed(0)),
	)

	require.NoError(t, response.UnmarshalBERTLV(tlv))

	assert.Empty(t, response.NotificationList)
	assert.NoError(t, response.Valid())
}

func TestRetrieveNotificationsListResponseErrorChoice(t *testing.T) {
	response := new(RetrieveNotificationsListResponse)
	tlv := bertlv.NewChildren(
		bertlv.ContextSpecific.Constructed(43),
		bertlv.NewValue(bertlv.ContextSpecific.Primitive(1), []byte{0x7f}),
	)

	err := response.UnmarshalBERTLV(tlv)

	assert.ErrorIs(t, err, ErrUndefined)
}

func TestNotificationEventRejectsInvalidBitCount(t *testing.T) {
	var event NotificationEvent

	assert.Error(t, event.UnmarshalBinary([]byte{0x04, 0x00}))
	assert.Error(t, event.UnmarshalBinary([]byte{0x04, 0xc0}))
	assert.Error(t, event.UnmarshalBinary([]byte{0x03, 0x08}))
}

func TestNotificationMetadataUsesUTF8StringAddressTag(t *testing.T) {
	metadata := new(NotificationMetadata)

	require.NoError(t, metadata.UnmarshalBERTLV(notificationMetadataTLV()))

	assert.Equal(t, SequenceNumber(1), metadata.SequenceNumber)
	assert.Equal(t, NotificationEventInstall, metadata.ProfileManagementOperation)
	assert.Equal(t, "example.com", metadata.Address)
}

func TestPendingNotificationUnmarshalProfileInstallationResult(t *testing.T) {
	tlv := bertlv.NewChildren(
		bertlv.ContextSpecific.Constructed(55),
		bertlv.NewChildren(
			bertlv.ContextSpecific.Constructed(39),
			notificationMetadataTLV(),
			bertlv.NewChildren(bertlv.ContextSpecific.Constructed(2)),
		),
		bertlv.NewValue(bertlv.Application.Primitive(55), []byte{0x01}),
	)
	notification := new(PendingNotification)

	require.NoError(t, notification.UnmarshalBERTLV(tlv))

	assert.Same(t, tlv, notification.PendingNotification)
	assert.Equal(t, "example.com", notification.Notification.Address)
}

func TestPendingNotificationUnmarshalOtherSignedNotification(t *testing.T) {
	tlv := bertlv.NewChildren(
		bertlv.Universal.Constructed(16),
		notificationMetadataTLV(),
		bertlv.NewValue(bertlv.Application.Primitive(55), []byte{0x01}),
		bertlv.NewChildren(bertlv.ContextSpecific.Constructed(2)),
		bertlv.NewChildren(bertlv.ContextSpecific.Constructed(3)),
	)
	notification := new(PendingNotification)

	require.NoError(t, notification.UnmarshalBERTLV(tlv))

	assert.Same(t, tlv, notification.PendingNotification)
	assert.Equal(t, "example.com", notification.Notification.Address)
}

func notificationMetadataTLV() *bertlv.TLV {
	return bertlv.NewChildren(
		bertlv.ContextSpecific.Constructed(47),
		bertlv.NewValue(bertlv.ContextSpecific.Primitive(0), []byte{0x01}),
		bertlv.NewValue(bertlv.ContextSpecific.Primitive(1), []byte{0x04, 0x80}),
		bertlv.NewValue(bertlv.Universal.Primitive(12), []byte("example.com")),
	)
}

func TestAuthenticateServerRequestRejectsInvalidIMEI(t *testing.T) {
	request := &AuthenticateServerRequest{IMEI: []byte{0x12, 0x34, 0x56}}

	_, err := request.MarshalBERTLV()

	assert.Error(t, err)
}

func TestAuthenticateServerRequestEncodesTACWithoutFullIMEI(t *testing.T) {
	identity, err := NewIMEI("35383741")
	require.NoError(t, err)

	request, err := (&AuthenticateServerRequest{
		IMEI:       identity,
		MatchingID: []byte("match"),
	}).MarshalBERTLV()
	require.NoError(t, err)

	expected := []byte{
		0xbf, 0x38, 0x13,
		0xa0, 0x11,
		0x80, 0x05, 'm', 'a', 't', 'c', 'h',
		0xa1, 0x08,
		0x80, 0x04, 0x35, 0x38, 0x37, 0x41,
		0xa1, 0x00,
	}
	assert.Equal(t, expected, request.Bytes())
}

func TestAuthenticateServerRequestEncodesTACAndFullIMEISeparately(t *testing.T) {
	identity, err := NewIMEI("353837410000001")
	require.NoError(t, err)

	request, err := (&AuthenticateServerRequest{
		IMEI:       identity,
		MatchingID: []byte("match"),
	}).MarshalBERTLV()
	require.NoError(t, err)

	expected := []byte{
		0xbf, 0x38, 0x1d,
		0xa0, 0x1b,
		0x80, 0x05, 'm', 'a', 't', 'c', 'h',
		0xa1, 0x12,
		0x80, 0x04, 0x35, 0x38, 0x37, 0x41,
		0xa1, 0x00,
		0x82, 0x08, 0x53, 0x83, 0x73, 0x14, 0x00, 0x00, 0x00, 0xf1,
	}
	assert.Equal(t, expected, request.Bytes())
}
