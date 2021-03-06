package connect

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"github.com/micromdm/mdm"
	"github.com/micromdm/micromdm/application"
	"github.com/micromdm/micromdm/certificate"
	"github.com/micromdm/micromdm/command"
	"github.com/micromdm/micromdm/device"
	"github.com/pkg/errors"
	"golang.org/x/net/context"
	"time"
)

// Service defines methods for an MDM service
type Service interface {
	Acknowledge(ctx context.Context, req mdm.Response) (int, error)
	NextCommand(ctx context.Context, req mdm.Response) ([]byte, int, error)
	FailCommand(ctx context.Context, req mdm.Response) (int, error)
}

// NewService creates a mdm service
func NewService(devices device.Datastore, apps application.Datastore, certs certificate.Datastore, cs command.Service) Service {
	return &service{
		commands: cs,
		devices:  devices,
		apps:     apps,
		certs:    certs,
	}
}

type service struct {
	devices  device.Datastore
	apps     application.Datastore
	commands command.Service
	certs    certificate.Datastore
}

// Acknowledge a response from a device.
// NOTE: IOS devices do not always include the key `RequestType` in their response. Only the presence of the
// result key can be used to identify the response (or the command UUID)
func (svc service) Acknowledge(ctx context.Context, req mdm.Response) (int, error) {
	requestPayload, err := svc.commands.Find(req.CommandUUID)

	switch requestPayload.Command.RequestType {
	case "DeviceInformation":
		if err := svc.ackQueryResponses(req); err != nil {
			return 0, err
		}
	case "InstalledApplicationList":
		if err := svc.ackInstalledApplicationList(req); err != nil {
			return 0, err
		}
	case "CertificateList":
		if err := svc.ackCertificateList(req); err != nil {
			return 0, err
		}
	default:
		// Unhandled MDM client response
	}

	total, err := svc.commands.DeleteCommand(req.UDID, req.CommandUUID)
	if err != nil {
		return total, err
	}
	if total == 0 {
		total, err = svc.checkRequeue(req.UDID)
		if err != nil {
			return total, err
		}
		return total, nil
	}
	return total, nil
}

func (svc service) NextCommand(ctx context.Context, req mdm.Response) ([]byte, int, error) {
	return svc.commands.NextCommand(req.UDID)
}

func (svc service) FailCommand(ctx context.Context, req mdm.Response) (int, error) {
	return svc.commands.DeleteCommand(req.UDID, req.CommandUUID)
}

func (svc service) checkRequeue(deviceUDID string) (int, error) {
	existing, err := svc.devices.GetDeviceByUDID(deviceUDID, []string{"awaiting_configuration"}...)
	if err != nil {
		return 0, errors.Wrap(err, "check and requeue")
	}
	if existing.AwaitingConfiguration {
		cmdRequest := &mdm.CommandRequest{
			UDID:        deviceUDID,
			RequestType: "DeviceConfigured",
		}
		_, err := svc.commands.NewCommand(cmdRequest)
		if err != nil {
			return 0, err
		}
		return 1, nil
	}
	return 0, nil
}

// Acknowledge Queries sent with DeviceInformation command
func (svc service) ackQueryResponses(req mdm.Response) error {
	devices, err := svc.devices.Devices(
		device.SerialNumber{SerialNumber: req.QueryResponses.SerialNumber},
		device.UDID{UDID: req.UDID},
	)

	if err != nil {
		return errors.Wrap(err, "ackQueryResponses fetching device")
	}

	if len(devices) == 0 {
		return errors.New("no enrolled device matches the one responding")
	}

	if len(devices) > 1 {
		return fmt.Errorf("expected a single device for udid: %s, serial number: %s, but got more than one.", req.UDID, req.QueryResponses.SerialNumber)
	}

	existing := devices[0]

	existing.LastCheckin = time.Now().UTC()
	existing.LastQueryResponse, err = json.Marshal(req.QueryResponses)

	if err != nil {
		return err
	}

	var serialNumber device.JsonNullString
	serialNumber.Scan(req.QueryResponses.SerialNumber)

	existing.ProductName = req.QueryResponses.ProductName
	existing.BuildVersion = req.QueryResponses.BuildVersion
	existing.DeviceName = req.QueryResponses.DeviceName
	existing.IMEI = req.QueryResponses.IMEI
	existing.MEID = req.QueryResponses.MEID
	existing.Model = req.QueryResponses.Model
	existing.OSVersion = req.QueryResponses.OSVersion
	existing.SerialNumber = serialNumber

	return svc.devices.Save("queryResponses", &existing)
}

// Acknowledge a response to `InstalledApplicationList`.
func (svc service) ackInstalledApplicationList(req mdm.Response) error {
	dev, err := svc.devices.GetDeviceByUDID(req.UDID, "device_uuid")
	if err != nil {
		return errors.Wrap(err, "getting a device record by udid")
	}

	if err := svc.apps.DeleteDeviceApplications(dev.UUID); err != nil {
		return fmt.Errorf("clearing applications for device: %s", err)
	}

	var requestApps []application.DeviceApplication = make([]application.DeviceApplication, len(req.InstalledApplicationList))
	// Update or insert application records that do not exist, returning the UUID so that it can be inserted for
	// the device sending the response.
	for i, reqApp := range req.InstalledApplicationList {
		identifier := sql.NullString{reqApp.Identifier, reqApp.Identifier != ""}
		shortVersion := sql.NullString{reqApp.ShortVersion, reqApp.ShortVersion != ""}
		version := sql.NullString{reqApp.Version, reqApp.Version != ""}

		bundleSize := sql.NullInt64{}
		bundleSize.Scan(reqApp.BundleSize)

		dynamicSize := sql.NullInt64{}
		dynamicSize.Scan(reqApp.DynamicSize)

		newApp := application.DeviceApplication{
			DeviceUUID:   dev.UUID,
			Name:         reqApp.Name,
			Identifier:   identifier,
			ShortVersion: shortVersion,
			Version:      version,
			BundleSize:   bundleSize,
			DynamicSize:  dynamicSize,
		}

		if err := svc.apps.NewDeviceApp(&newApp); err != nil {
			fmt.Println(err)
			fmt.Printf("%v\n", newApp)
			return fmt.Errorf("inserting an application for device: %s", err)
		}

		requestApps[i] = newApp
		//fmt.Printf("%v\n", newApp)
	}

	//fmt.Printf("%v\n", requestApps)

	return nil
}

// Acknowledge a response to `CertificateList`.
func (svc service) ackCertificateList(req mdm.Response) error {
	device, err := svc.devices.GetDeviceByUDID(req.UDID, "device_uuid")
	if err != nil {
		return errors.Wrap(err, "getting a device record by udid")
	}

	var certs []certificate.Certificate = []certificate.Certificate{}
	for _, cert := range req.CertificateList {
		newCert := certificate.Certificate{
			CommonName: cert.CommonName,
			IsIdentity: cert.IsIdentity,
			Data:       cert.Data,
			DeviceUUID: device.UUID,
		}

		certs = append(certs, newCert)
	}

	if err := svc.certs.ReplaceCertificatesByDeviceUUID(device.UUID, certs); err != nil {
		return err
	}

	return nil
}
