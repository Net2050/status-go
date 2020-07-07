package push_notification_server

import (
	"context"
	"crypto/ecdsa"
	"errors"

	"github.com/golang/protobuf/proto"
	"github.com/google/uuid"

	"github.com/status-im/status-go/eth-node/crypto/ecies"
	"github.com/status-im/status-go/protocol/common"
	"github.com/status-im/status-go/protocol/protobuf"
	"go.uber.org/zap"
)

const encryptedPayloadKeyLength = 16

type Config struct {
	// Identity is our identity key
	Identity *ecdsa.PrivateKey
	// GorushUrl is the url for the gorush service
	GorushURL string

	Logger *zap.Logger
}

type Server struct {
	persistence      Persistence
	config           *Config
	messageProcessor *common.MessageProcessor
}

func New(config *Config, persistence Persistence, messageProcessor *common.MessageProcessor) *Server {
	return &Server{persistence: persistence, config: config, messageProcessor: messageProcessor}
}

func (p *Server) generateSharedKey(publicKey *ecdsa.PublicKey) ([]byte, error) {
	return ecies.ImportECDSA(p.config.Identity).GenerateShared(
		ecies.ImportECDSAPublic(publicKey),
		encryptedPayloadKeyLength,
		encryptedPayloadKeyLength,
	)
}

func (p *Server) validateUUID(u string) error {
	if len(u) == 0 {
		return errors.New("empty uuid")
	}
	_, err := uuid.Parse(u)
	return err
}

func (p *Server) decryptRegistration(publicKey *ecdsa.PublicKey, payload []byte) ([]byte, error) {
	sharedKey, err := p.generateSharedKey(publicKey)
	if err != nil {
		return nil, err
	}

	return common.Decrypt(payload, sharedKey)
}

// ValidateRegistration validates a new message against the last one received for a given installationID and and public key
// and return the decrypted message
func (p *Server) ValidateRegistration(publicKey *ecdsa.PublicKey, payload []byte) (*protobuf.PushNotificationRegistration, error) {
	if payload == nil {
		return nil, ErrEmptyPushNotificationRegistrationPayload
	}

	if publicKey == nil {
		return nil, ErrEmptyPushNotificationRegistrationPublicKey
	}

	decryptedPayload, err := p.decryptRegistration(publicKey, payload)
	if err != nil {
		return nil, err
	}

	registration := &protobuf.PushNotificationRegistration{}

	if err := proto.Unmarshal(decryptedPayload, registration); err != nil {
		return nil, ErrCouldNotUnmarshalPushNotificationRegistration
	}

	if registration.Version < 1 {
		return nil, ErrInvalidPushNotificationRegistrationVersion
	}

	if err := p.validateUUID(registration.InstallationId); err != nil {
		return nil, ErrMalformedPushNotificationRegistrationInstallationID
	}

	previousRegistration, err := p.persistence.GetPushNotificationRegistrationByPublicKeyAndInstallationID(common.HashPublicKey(publicKey), registration.InstallationId)
	if err != nil {
		return nil, err
	}

	if previousRegistration != nil && registration.Version <= previousRegistration.Version {
		return nil, ErrInvalidPushNotificationRegistrationVersion
	}

	// Unregistering message
	if registration.Unregister {
		return registration, nil
	}

	if err := p.validateUUID(registration.AccessToken); err != nil {
		return nil, ErrMalformedPushNotificationRegistrationAccessToken
	}

	if len(registration.Token) == 0 {
		return nil, ErrMalformedPushNotificationRegistrationDeviceToken
	}

	if registration.TokenType == protobuf.PushNotificationRegistration_UNKNOWN_TOKEN_TYPE {
		return nil, ErrUnknownPushNotificationRegistrationTokenType
	}

	return registration, nil
}

func (p *Server) HandlePushNotificationQuery(query *protobuf.PushNotificationQuery) *protobuf.PushNotificationQueryResponse {
	response := &protobuf.PushNotificationQueryResponse{}
	if query == nil || len(query.PublicKeys) == 0 {
		return response
	}

	registrations, err := p.persistence.GetPushNotificationRegistrationByPublicKeys(query.PublicKeys)
	if err != nil {
		// TODO: log errors
		return response
	}

	for _, idAndResponse := range registrations {

		registration := idAndResponse.Registration
		info := &protobuf.PushNotificationQueryInfo{
			PublicKey:      idAndResponse.ID,
			InstallationId: registration.InstallationId,
		}

		if len(registration.AllowedUserList) > 0 {
			info.AllowedUserList = registration.AllowedUserList
		} else {
			info.AccessToken = registration.AccessToken
		}
		response.Info = append(response.Info, info)
	}

	response.Success = true
	return response
}

func (p *Server) HandlePushNotificationRequest(request *protobuf.PushNotificationRequest) *protobuf.PushNotificationResponse {
	response := &protobuf.PushNotificationResponse{}
	// We don't even send a response in this case
	if request == nil || len(request.MessageId) == 0 {
		return nil
	}

	response.MessageId = request.MessageId

	// Collect successful requests & registrations
	var requestAndRegistrations []*RequestAndRegistration

	for _, pn := range request.Requests {
		registration, err := p.persistence.GetPushNotificationRegistrationByPublicKeyAndInstallationID(pn.PublicKey, pn.InstallationId)
		report := &protobuf.PushNotificationReport{
			PublicKey:      pn.PublicKey,
			InstallationId: pn.InstallationId,
		}

		if err != nil {
			// TODO: log error
			report.Error = protobuf.PushNotificationReport_UNKNOWN_ERROR_TYPE
		} else if registration == nil {
			report.Error = protobuf.PushNotificationReport_NOT_REGISTERED
		} else if registration.AccessToken != pn.AccessToken {
			report.Error = protobuf.PushNotificationReport_WRONG_TOKEN
		} else {
			// For now we just assume that the notification will be successful
			requestAndRegistrations = append(requestAndRegistrations, &RequestAndRegistration{
				Request:      pn,
				Registration: registration,
			})
			report.Success = true
		}

		response.Reports = append(response.Reports, report)
	}

	if len(requestAndRegistrations) == 0 {
		return response
	}

	// This can be done asynchronously
	goRushRequest := PushNotificationRegistrationToGoRushRequest(requestAndRegistrations)
	err := sendGoRushNotification(goRushRequest, p.config.GorushURL)
	if err != nil {
		// TODO: handle this error?
	}

	return response
}

func (s *Server) HandlePushNotificationRegistration(publicKey *ecdsa.PublicKey, payload []byte) *protobuf.PushNotificationRegistrationResponse {

	s.config.Logger.Debug("handling push notification registration")
	response := &protobuf.PushNotificationRegistrationResponse{
		RequestId: common.Shake256(payload),
	}

	registration, err := s.ValidateRegistration(publicKey, payload)

	if err != nil {
		if err == ErrInvalidPushNotificationRegistrationVersion {
			response.Error = protobuf.PushNotificationRegistrationResponse_VERSION_MISMATCH
		} else {
			response.Error = protobuf.PushNotificationRegistrationResponse_MALFORMED_MESSAGE
		}
		s.config.Logger.Warn("registration did not validate", zap.Error(err))
		return response
	}

	if registration.Unregister {
		// We save an empty registration, only keeping version and installation-id
		emptyRegistration := &protobuf.PushNotificationRegistration{
			Version:        registration.Version,
			InstallationId: registration.InstallationId,
		}
		if err := s.persistence.SavePushNotificationRegistration(common.HashPublicKey(publicKey), emptyRegistration); err != nil {
			response.Error = protobuf.PushNotificationRegistrationResponse_INTERNAL_ERROR
			s.config.Logger.Error("failed to unregister ", zap.Error(err))
			return response
		}

	} else if err := s.persistence.SavePushNotificationRegistration(common.HashPublicKey(publicKey), registration); err != nil {
		response.Error = protobuf.PushNotificationRegistrationResponse_INTERNAL_ERROR
		s.config.Logger.Error("failed to save registration", zap.Error(err))
		return response
	}

	response.Success = true

	s.config.Logger.Debug("handled push notification registration successfully")

	return response
}

func (p *Server) HandlePushNotificationRegistration2(publicKey *ecdsa.PublicKey, payload []byte) error {
	response := p.HandlePushNotificationRegistration(publicKey, payload)
	if response == nil {
		return nil
	}
	encodedMessage, err := proto.Marshal(response)
	if err != nil {
		return err
	}

	rawMessage := &common.RawMessage{
		Payload:     encodedMessage,
		MessageType: protobuf.ApplicationMetadataMessage_PUSH_NOTIFICATION_REGISTRATION_RESPONSE,
	}

	_, err = p.messageProcessor.SendPrivate(context.Background(), publicKey, rawMessage)
	return err
}

func (p *Server) HandlePushNotificationQuery2(publicKey *ecdsa.PublicKey, query protobuf.PushNotificationQuery) error {
	response := p.HandlePushNotificationQuery(&query)
	if response == nil {
		return nil
	}
	encodedMessage, err := proto.Marshal(response)
	if err != nil {
		return err
	}

	rawMessage := &common.RawMessage{
		Payload:     encodedMessage,
		MessageType: protobuf.ApplicationMetadataMessage_PUSH_NOTIFICATION_QUERY_RESPONSE,
	}

	_, err = p.messageProcessor.SendPrivate(context.Background(), publicKey, rawMessage)
	return err

}

func (p *Server) HandlePushNotificationRequest2(publicKey *ecdsa.PublicKey,
	request protobuf.PushNotificationRequest) error {
	response := p.HandlePushNotificationRequest(&request)
	if response == nil {
		return nil
	}
	encodedMessage, err := proto.Marshal(response)
	if err != nil {
		return err
	}

	rawMessage := &common.RawMessage{
		Payload:     encodedMessage,
		MessageType: protobuf.ApplicationMetadataMessage_PUSH_NOTIFICATION_RESPONSE,
	}

	_, err = p.messageProcessor.SendPrivate(context.Background(), publicKey, rawMessage)
	return err
}