package vehicle

// LICENSE

// Copyright (c) 2019-2022 andig

// This module is NOT covered by the MIT license. All rights reserved.

// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.

// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/provider"
	"github.com/evcc-io/evcc/util"
	"github.com/evcc-io/evcc/util/oauth"
	"github.com/evcc-io/evcc/util/request"
	"github.com/evcc-io/evcc/util/sponsor"
	"github.com/evcc-io/evcc/vehicle/tronity"
	"golang.org/x/oauth2"
)

// Tronity is an api.Vehicle implementation for the Tronity api
type Tronity struct {
	*embed
	*request.Helper
	oc    *oauth2.Config
	vid   string
	bulkG func() (tronity.Bulk, error)
}

func init() {
	registry.Add("tronity", NewTronityFromConfig)
}

// go:generate go run ../cmd/tools/decorate.go -f decorateTronity -b *Tronity -r api.Vehicle -t "api.ChargeState,Status,func() (api.ChargeStatus, error)" -t "api.VehicleOdometer,Odometer,func() (float64, error)" -t "api.VehicleChargeController,StartCharge,func() error" -t "api.VehicleChargeController,StopCharge,func() error"

// NewTronityFromConfig creates a new vehicle
func NewTronityFromConfig(other map[string]interface{}) (api.Vehicle, error) {
	cc := struct {
		embed       `mapstructure:",squash"`
		Credentials ClientCredentials
		Tokens      Tokens
		VIN         string
		Cache       time.Duration
	}{
		Cache: interval,
	}

	if err := util.DecodeOther(other, &cc); err != nil {
		return nil, err
	}

	if err := cc.Credentials.Error(); err != nil {
		return nil, err
	}

	if !sponsor.IsAuthorized() {
		return nil, api.ErrSponsorRequired
	}

	// authenticated http client with logging injected to the tronity client
	log := util.NewLogger("tronity").Redact(cc.Credentials.ID, cc.Credentials.Secret)

	oc, err := tronity.OAuth2Config(cc.Credentials.ID, cc.Credentials.Secret)
	if err != nil {
		return nil, err
	}

	v := &Tronity{
		embed:  &cc.embed,
		Helper: request.NewHelper(log),
		oc:     oc,
	}

	var ts oauth2.TokenSource
	token, err := cc.Tokens.Token()

	// https://app.platform.tronity.io/docs#tag/Authentication
	if err != nil {
		// use app flow if we don't have tokens
		ts = oauth.RefreshTokenSource(nil, v)
	} else {
		// use provided tokens generated by code flow
		ctx := context.WithValue(context.Background(), oauth2.HTTPClient, request.NewClient(log))
		ts = oc.TokenSource(ctx, token)
	}

	// replace client transport with authenticated transport
	v.Client.Transport = &oauth2.Transport{
		Source: ts,
		Base:   v.Client.Transport,
	}

	vehicle, err := ensureVehicleEx(
		cc.VIN, v.vehicles,
		func(v tronity.Vehicle) string {
			return v.VIN
		},
	)
	if err != nil {
		return nil, err
	}

	v.vid = vehicle.ID
	v.bulkG = provider.Cached(v.bulk, cc.Cache)

	var status func() (api.ChargeStatus, error)
	if slices.Contains(vehicle.Scopes, tronity.ReadCharge) {
		status = v.status
	}

	var odometer func() (float64, error)
	if slices.Contains(vehicle.Scopes, tronity.ReadOdometer) {
		odometer = v.odometer
	}

	var start, stop func() error
	if slices.Contains(vehicle.Scopes, tronity.WriteChargeStartStop) {
		start = v.startCharge
		stop = v.stopCharge
	}

	return decorateTronity(v, status, odometer, start, stop), nil
}

// RefreshToken performs token refresh by logging in with app context
func (v *Tronity) RefreshToken(_ *oauth2.Token) (*oauth2.Token, error) {
	data := struct {
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
		GrantType    string `json:"grant_type"`
	}{
		ClientID:     v.oc.ClientID,
		ClientSecret: v.oc.ClientSecret,
		GrantType:    "app",
	}

	req, _ := request.New(http.MethodPost, v.oc.Endpoint.TokenURL, request.MarshalJSON(data), request.JSONEncoding)

	var token oauth.Token
	err := v.DoJSON(req, &token)

	return (*oauth2.Token)(&token), err
}

// vehicles implements the vehicles api
func (v *Tronity) vehicles() ([]tronity.Vehicle, error) {
	uri := fmt.Sprintf("%s/tronity/vehicles", tronity.URI)

	var res tronity.Vehicles
	err := v.GetJSON(uri, &res)

	return res.Data, err
}

// bulk implements the bulk api
func (v *Tronity) bulk() (tronity.Bulk, error) {
	uri := fmt.Sprintf("%s/tronity/vehicles/%s/last_record", tronity.URI, v.vid)

	var res tronity.Bulk
	err := v.GetJSON(uri, &res)

	return res, err
}

// Soc implements the api.Vehicle interface
func (v *Tronity) Soc() (float64, error) {
	res, err := v.bulkG()
	return res.Level, err
}

// status implements the api.ChargeState interface
func (v *Tronity) status() (api.ChargeStatus, error) {
	status := api.StatusA // disconnected
	res, err := v.bulkG()
	if err != nil {
		return status, err
	}

	switch {
	case res.Charging == "Charging":
		status = api.StatusC
	case res.Plugged:
		status = api.StatusB
	}

	return status, nil
}

var _ api.VehicleRange = (*Tronity)(nil)

// Range implements the api.VehicleRange interface
func (v *Tronity) Range() (int64, error) {
	res, err := v.bulkG()
	return int64(res.Range), err
}

// odometer implements the api.VehicleOdometer interface
func (v *Tronity) odometer() (float64, error) {
	res, err := v.bulkG()
	return res.Odometer, err
}

func (v *Tronity) post(uri string) error {
	resp, err := v.Post(uri, "", nil)
	if err == nil {
		err = request.ResponseError(resp)
	}

	// ignore HTTP 405
	if err != nil {
		if err2, ok := err.(request.StatusError); ok && err2.HasStatus(http.StatusMethodNotAllowed) {
			err = nil
		}
	}

	return err
}

// startCharge implements the api.VehicleChargeController interface
func (v *Tronity) startCharge() error {
	uri := fmt.Sprintf("%s/tronity/vehicles/%s/start_charging", tronity.URI, v.vid)
	return v.post(uri)
}

// stopCharge implements the api.VehicleChargeController interface
func (v *Tronity) stopCharge() error {
	uri := fmt.Sprintf("%s/tronity/vehicles/%s/stop_charging", tronity.URI, v.vid)
	return v.post(uri)
}
