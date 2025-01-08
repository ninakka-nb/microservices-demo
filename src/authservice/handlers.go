// Copyright 2018 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"bytes"
	"context"
	"crypto/md5"
	"database/sql"
	"encoding/hex"
	"fmt"
	"html/template"
	"io"
	"math/rand"
	"net/http"
	"net/mail"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/mux"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"

	pb "github.com/GoogleCloudPlatform/microservices-demo/src/authservice/genproto"
	"github.com/GoogleCloudPlatform/microservices-demo/src/authservice/money"

	"golang.org/x/crypto/bcrypt"
)

type platformDetails struct {
	css      string
	provider string
}

var (
	isCymbalBrand = "true" == strings.ToLower(os.Getenv("CYMBAL_BRANDING"))
	templates     = template.Must(template.New("").
			Funcs(template.FuncMap{
			"renderMoney":        renderMoney,
			"renderCurrencyLogo": renderCurrencyLogo,
		}).ParseGlob("login_templates/*.html"))
	plat platformDetails
)

var validEnvs = []string{"local", "gcp", "azure", "aws", "onprem", "alibaba"}

func (fe *frontendServer) homeHandler(w http.ResponseWriter, r *http.Request) {
	log := r.Context().Value(ctxKeyLog{}).(logrus.FieldLogger)

	log.Debugf("Processing request %v", r.URL)

	if !isAuthenticated(r, log) {
		log.Info("User is not authenticated. Redirecting to login page.")
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}

	log.Debugf("Proxying request %v to frontend:80", r.URL)
	// we need to buffer the body if we want to read it here and send it
	// in the request.
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// you can reassign the body if you need to parse it as multipart
	r.Body = io.NopCloser(bytes.NewReader(body))

	// create a new url from the raw RequestURI sent by the client
	url := fmt.Sprintf("http://frontend:80%s", r.RequestURI)

	proxyReq, err := http.NewRequest(r.Method, url, bytes.NewReader(body))

	// We may want to filter some headers, otherwise we could just use a shallow copy
	// proxyReq.Header = req.Header
	proxyReq.Header = make(http.Header)
	for h, val := range r.Header {
		proxyReq.Header[h] = val
	}

	httpClient := http.Client{}
	resp, err := httpClient.Do(proxyReq)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// Dummy sleep to mimic realistic API scenario
	// time.Sleep(50 * time.Millisecond)

	// Step 4: copy payload to response writer
	copyHeader(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

// copyHeader and singleJoiningSlash are copy from "/net/http/httputil/reverseproxy.go"
func copyHeader(dst, src http.Header) {
	for k, vv := range src {
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

func (plat *platformDetails) setPlatformDetails(env string) {
	if env == "aws" {
		plat.provider = "AWS"
		plat.css = "aws-platform"
	} else if env == "onprem" {
		plat.provider = "On-Premises"
		plat.css = "onprem-platform"
	} else if env == "azure" {
		plat.provider = "Azure"
		plat.css = "azure-platform"
	} else if env == "gcp" {
		plat.provider = "Google Cloud"
		plat.css = "gcp-platform"
	} else if env == "alibaba" {
		plat.provider = "Alibaba Cloud"
		plat.css = "alibaba-platform"
	} else {
		plat.provider = "local"
		plat.css = "local"
	}
}

func (fe *frontendServer) productHandler(w http.ResponseWriter, r *http.Request) {
	log := r.Context().Value(ctxKeyLog{}).(logrus.FieldLogger)
	id := mux.Vars(r)["id"]
	if id == "" {
		renderHTTPError(log, r, w, errors.New("product id not specified"), http.StatusBadRequest)
		return
	}
	log.WithField("id", id).WithField("currency", currentCurrency(r)).
		Debug("serving product page")

	p, err := fe.getProduct(r.Context(), id)
	if err != nil {
		renderHTTPError(log, r, w, errors.Wrap(err, "could not retrieve product"), http.StatusInternalServerError)
		return
	}
	currencies, err := fe.getCurrencies(r.Context())
	if err != nil {
		renderHTTPError(log, r, w, errors.Wrap(err, "could not retrieve currencies"), http.StatusInternalServerError)
		return
	}

	cart, err := fe.getCart(r.Context(), sessionID(r))
	if err != nil {
		renderHTTPError(log, r, w, errors.Wrap(err, "could not retrieve cart"), http.StatusInternalServerError)
		return
	}

	price, err := fe.convertCurrency(r.Context(), p.GetPriceUsd(), currentCurrency(r))
	if err != nil {
		renderHTTPError(log, r, w, errors.Wrap(err, "failed to convert currency"), http.StatusInternalServerError)
		return
	}

	// ignores the error retrieving recommendations since it is not critical
	recommendations, err := fe.getRecommendations(r.Context(), sessionID(r), []string{id}, r.Header.Get("Tenantname"))
	if err != nil {
		log.WithField("error", err).Warn("failed to get product recommendations")
	}

	product := struct {
		Item  *pb.Product
		Price *pb.Money
	}{p, price}

	if err := templates.ExecuteTemplate(w, "product", map[string]interface{}{
		"session_id":        sessionID(r),
		"request_id":        r.Context().Value(ctxKeyRequestID{}),
		"ad":                fe.chooseAd(r.Context(), p.Categories, log),
		"user_currency":     currentCurrency(r),
		"show_currency":     true,
		"currencies":        currencies,
		"product":           product,
		"recommendations":   recommendations,
		"cart_size":         cartSize(cart),
		"platform_css":      plat.css,
		"platform_name":     plat.provider,
		"is_cymbal_brand":   isCymbalBrand,
		"deploymentDetails": deploymentDetailsMap,
	}); err != nil {
		log.Println(err)
	}
}

func (fe *frontendServer) addToCartHandler(w http.ResponseWriter, r *http.Request) {
	log := r.Context().Value(ctxKeyLog{}).(logrus.FieldLogger)
	quantity, _ := strconv.ParseUint(r.FormValue("quantity"), 10, 32)
	productID := r.FormValue("product_id")
	if productID == "" || quantity == 0 {
		renderHTTPError(log, r, w, errors.New("invalid form input"), http.StatusBadRequest)
		return
	}
	log.WithField("product", productID).WithField("quantity", quantity).Debug("adding to cart")

	p, err := fe.getProduct(r.Context(), productID)
	if err != nil {
		renderHTTPError(log, r, w, errors.Wrap(err, "could not retrieve product"), http.StatusInternalServerError)
		return
	}

	if err := fe.insertCart(r.Context(), sessionID(r), p.GetId(), int32(quantity)); err != nil {
		renderHTTPError(log, r, w, errors.Wrap(err, "failed to add to cart"), http.StatusInternalServerError)
		return
	}
	w.Header().Set("location", "/cart")
	w.WriteHeader(http.StatusFound)
}

func (fe *frontendServer) emptyCartHandler(w http.ResponseWriter, r *http.Request) {
	log := r.Context().Value(ctxKeyLog{}).(logrus.FieldLogger)
	log.Debug("emptying cart")

	if err := fe.emptyCart(r.Context(), sessionID(r)); err != nil {
		renderHTTPError(log, r, w, errors.Wrap(err, "failed to empty cart"), http.StatusInternalServerError)
		return
	}
	w.Header().Set("location", "/")
	w.WriteHeader(http.StatusFound)
}

func (fe *frontendServer) viewCartHandler(w http.ResponseWriter, r *http.Request) {
	log := r.Context().Value(ctxKeyLog{}).(logrus.FieldLogger)
	log.Debug("view user cart")

	if !isAuthenticated(r, log) {
		log.Info("User is not authenticated. Redirecting to login page")
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}

	currencies, err := fe.getCurrencies(r.Context())
	if err != nil {
		renderHTTPError(log, r, w, errors.Wrap(err, "could not retrieve currencies"), http.StatusInternalServerError)
		return
	}
	cart, err := fe.getCart(r.Context(), sessionID(r))
	if err != nil {
		renderHTTPError(log, r, w, errors.Wrap(err, "could not retrieve cart"), http.StatusInternalServerError)
		return
	}

	// ignores the error retrieving recommendations since it is not critical
	recommendations, err := fe.getRecommendations(r.Context(), sessionID(r), cartIDs(cart), r.Header.Get("Tenantname"))
	if err != nil {
		log.WithField("error", err).Warn("failed to get product recommendations")
	}

	shippingCost, err := fe.getShippingQuote(r.Context(), cart, currentCurrency(r), r.Header.Get("Tenantname"))
	if err != nil {
		renderHTTPError(log, r, w, errors.Wrap(err, "failed to get shipping quote"), http.StatusInternalServerError)
		return
	}

	type cartItemView struct {
		Item     *pb.Product
		Quantity int32
		Price    *pb.Money
	}
	items := make([]cartItemView, len(cart))
	totalPrice := pb.Money{CurrencyCode: currentCurrency(r)}
	for i, item := range cart {
		p, err := fe.getProduct(r.Context(), item.GetProductId())
		if err != nil {
			renderHTTPError(log, r, w, errors.Wrapf(err, "could not retrieve product #%s", item.GetProductId()), http.StatusInternalServerError)
			return
		}
		price, err := fe.convertCurrency(r.Context(), p.GetPriceUsd(), currentCurrency(r))
		if err != nil {
			renderHTTPError(log, r, w, errors.Wrapf(err, "could not convert currency for product #%s", item.GetProductId()), http.StatusInternalServerError)
			return
		}

		multPrice := money.MultiplySlow(*price, uint32(item.GetQuantity()))
		items[i] = cartItemView{
			Item:     p,
			Quantity: item.GetQuantity(),
			Price:    &multPrice}
		totalPrice = money.Must(money.Sum(totalPrice, multPrice))
	}
	totalPrice = money.Must(money.Sum(totalPrice, *shippingCost))
	year := time.Now().Year()

	if err := templates.ExecuteTemplate(w, "cart", map[string]interface{}{
		"session_id":        sessionID(r),
		"request_id":        r.Context().Value(ctxKeyRequestID{}),
		"user_currency":     currentCurrency(r),
		"currencies":        currencies,
		"recommendations":   recommendations,
		"cart_size":         cartSize(cart),
		"shipping_cost":     shippingCost,
		"show_currency":     true,
		"total_cost":        totalPrice,
		"items":             items,
		"expiration_years":  []int{year, year + 1, year + 2, year + 3, year + 4},
		"platform_css":      plat.css,
		"platform_name":     plat.provider,
		"is_cymbal_brand":   isCymbalBrand,
		"deploymentDetails": deploymentDetailsMap,
	}); err != nil {
		log.Println(err)
	}
}

func (fe *frontendServer) placeOrderHandler(w http.ResponseWriter, r *http.Request) {
	log := r.Context().Value(ctxKeyLog{}).(logrus.FieldLogger)
	log.Debug("placing order")

	var (
		email         = r.FormValue("email")
		streetAddress = r.FormValue("street_address")
		zipCode, _    = strconv.ParseInt(r.FormValue("zip_code"), 10, 32)
		city          = r.FormValue("city")
		state         = r.FormValue("state")
		country       = r.FormValue("country")
		ccNumber      = r.FormValue("credit_card_number")
		ccMonth, _    = strconv.ParseInt(r.FormValue("credit_card_expiration_month"), 10, 32)
		ccYear, _     = strconv.ParseInt(r.FormValue("credit_card_expiration_year"), 10, 32)
		ccCVV, _      = strconv.ParseInt(r.FormValue("credit_card_cvv"), 10, 32)
	)

	order, err := pb.NewCheckoutServiceClient(fe.checkoutSvcConn).
		PlaceOrder(r.Context(), &pb.PlaceOrderRequest{
			Email: email,
			CreditCard: &pb.CreditCardInfo{
				CreditCardNumber:          ccNumber,
				CreditCardExpirationMonth: int32(ccMonth),
				CreditCardExpirationYear:  int32(ccYear),
				CreditCardCvv:             int32(ccCVV)},
			UserId:       sessionID(r),
			UserCurrency: currentCurrency(r),
			Address: &pb.Address{
				StreetAddress: streetAddress,
				City:          city,
				State:         state,
				ZipCode:       int32(zipCode),
				Country:       country},
		})
	if err != nil {
		renderHTTPError(log, r, w, errors.Wrap(err, "failed to complete the order"), http.StatusInternalServerError)
		return
	}
	log.WithField("order", order.GetOrder().GetOrderId()).Info("order placed")

	order.GetOrder().GetItems()
	recommendations, _ := fe.getRecommendations(r.Context(), sessionID(r), nil, r.Header.Get("Tenantname"))

	totalPaid := *order.GetOrder().GetShippingCost()
	for _, v := range order.GetOrder().GetItems() {
		multPrice := money.MultiplySlow(*v.GetCost(), uint32(v.GetItem().GetQuantity()))
		totalPaid = money.Must(money.Sum(totalPaid, multPrice))
	}

	currencies, err := fe.getCurrencies(r.Context())
	if err != nil {
		renderHTTPError(log, r, w, errors.Wrap(err, "could not retrieve currencies"), http.StatusInternalServerError)
		return
	}

	if err := templates.ExecuteTemplate(w, "order", map[string]interface{}{
		"session_id":        sessionID(r),
		"request_id":        r.Context().Value(ctxKeyRequestID{}),
		"user_currency":     currentCurrency(r),
		"show_currency":     false,
		"currencies":        currencies,
		"order":             order.GetOrder(),
		"total_paid":        &totalPaid,
		"recommendations":   recommendations,
		"platform_css":      plat.css,
		"platform_name":     plat.provider,
		"is_cymbal_brand":   isCymbalBrand,
		"deploymentDetails": deploymentDetailsMap,
	}); err != nil {
		log.Println(err)
	}
}

func (fe *frontendServer) loginHandler(w http.ResponseWriter, r *http.Request) {
	log := r.Context().Value(ctxKeyLog{}).(logrus.FieldLogger)
	log.Debug("login screen")

	var tpl bytes.Buffer
	err := templates.ExecuteTemplate(&tpl, "login", map[string]interface{}{
		"session_id":    sessionID(r),
		"request_id":    r.Context().Value(ctxKeyRequestID{}),
		"user_currency": currentCurrency(r),
		"show_currency": false,
		/*
			"currencies":        currencies,
			"order":             order.GetOrder(),
			"total_paid":        &totalPaid,
			"recommendations":   recommendations,
			"platform_css":      plat.css,
			"platform_name":     plat.provider,
			"is_cymbal_brand":   isCymbalBrand,
			"deploymentDetails": deploymentDetailsMap,
		*/
	})

	// log.Debug("Test printf")
	// log.Debug(tpl.String())

	if err = templates.ExecuteTemplate(w, "login", map[string]interface{}{
		"session_id":    sessionID(r),
		"request_id":    r.Context().Value(ctxKeyRequestID{}),
		"user_currency": currentCurrency(r),
		"show_currency": false,
		/*
			"currencies":        currencies,
			"order":             order.GetOrder(),
			"total_paid":        &totalPaid,
			"recommendations":   recommendations,
			"platform_css":      plat.css,
			"platform_name":     plat.provider,
			"is_cymbal_brand":   isCymbalBrand,
			"deploymentDetails": deploymentDetailsMap,
		*/
	}); err != nil {
		log.Println(err)
	}

	/*
		for _, c := range r.Cookies() {
			c.Expires = time.Now().Add(-time.Hour * 24 * 365)
			c.MaxAge = -1
			http.SetCookie(w, c)
		}
		w.Header().Set("Location", "/")
		w.WriteHeader(http.StatusFound)
	*/
}

func (fe *frontendServer) logoutHandler(w http.ResponseWriter, r *http.Request) {
	log := r.Context().Value(ctxKeyLog{}).(logrus.FieldLogger)
	log.Debug("logging out")
	for _, c := range r.Cookies() {
		c.Expires = time.Now().Add(-time.Hour * 24 * 365)
		c.MaxAge = -1
		http.SetCookie(w, c)
	}
	w.Header().Set("Location", "/")
	w.WriteHeader(http.StatusFound)
}

func (fe *frontendServer) localLoginHandler(w http.ResponseWriter, r *http.Request) {
	log := r.Context().Value(ctxKeyLog{}).(logrus.FieldLogger)

	log.Debugf("Method: %v", r.Method)

	// Loop over header names
	for name1, values1 := range r.Header {
		// Loop over all values for the name.
		for _, value1 := range values1 {
			log.Debugf("Header Name: %+v, Value: %+v", name1, value1)
		}
	}

	log.Debugf("local login handler")
	err := r.ParseForm()
	if err != nil {
		log.Errorf("Unable to parse login form. Err: %s", err.Error())
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}

	// Loop over header names
	for name2, values2 := range r.PostForm {
		// Loop over all values for the name.
		for _, value2 := range values2 {
			log.Debugf("Form Name: %+v, Value: %+v", name2, value2)
		}
	}

	email := r.FormValue("signinemail")
	_, err = mail.ParseAddress(email)
	if email == "" || err != nil {
		log.Errorf("Email field is empty or has an invalid email address string. Error: %s", err.Error())
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}

	password := r.FormValue("signinpwd")
	log.Infof("Email;Password provided is: %v;%v", email, password)
	if password == "" {
		log.Errorf("Password field cannot be empty.")
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}

	getEmailQuery := `SELECT * from userInfo where email=?`
	userRow := db.QueryRow(getEmailQuery, email)
	var uInfo UserInfo
	err = userRow.Scan(&uInfo.loginType, &uInfo.name, &uInfo.email, &uInfo.password)
	if err == sql.ErrNoRows {
		log.Errorf("Email address %s is not found. Please register.", email)
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	} else if err != nil {
		log.Errorf("Unable to get data from db. %+v(%s)", err, err.Error())
	}

	log.Debugf("Data from userInfo table, %+v", uInfo)

	err = bcrypt.CompareHashAndPassword([]byte(uInfo.password), []byte(password))
	if err != nil {
		log.Infof("Password does not match!")
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	} else {
		log.Infof("User successfully logged in!")
		// 2. Implement a success handler to issue some form of session
		session := sessionStore.New(sessionName)
		session.Set(sessionLoginType, "Local")
		hash := md5.Sum([]byte("Local" + uInfo.name + uInfo.email))
		session.Set(sessionUserKey, hex.EncodeToString(hash[:]))
		session.Set(sessionUsername, uInfo.name)
		session.Set(sessionUserEmail, uInfo.email)
		log.Debugf("session: %+v", session)
		if err := session.Save(w); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		http.Redirect(w, r, "/", http.StatusFound)
	}
}

func (fe *frontendServer) localRegisterHandler(w http.ResponseWriter, r *http.Request) {
	log := r.Context().Value(ctxKeyLog{}).(logrus.FieldLogger)

	log.Debugf("Method: %v", r.Method)

	// Loop over header names
	for name1, values1 := range r.Header {
		// Loop over all values for the name.
		for _, value1 := range values1 {
			log.Infof("Header Name: %+v, Value: %+v", name1, value1)
		}
	}

	log.Debug("local register handler")
	err := r.ParseForm()
	if err != nil {
		log.Infof("Unable to parse login form. Err: %s", err.Error())
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}

	// Loop over header names
	for name2, values2 := range r.PostForm {
		// Loop over all values for the name.
		for _, value2 := range values2 {
			log.Infof("Form Name: %+v, Value: %+v", name2, value2)
		}
	}

	name := r.FormValue("signupname")
	email := r.FormValue("signupemail")
	_, err = mail.ParseAddress(email)
	if email == "" || err != nil {
		log.Infof("Email field is empty or has an invalid email address string. Error: %s", err.Error())
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}

	password := r.FormValue("signuppwd")
	log.Infof("Name;Email;Password provided is: %v;%v;%v", name, email, password)
	if password == "" {
		log.Infof("Password field cannot be empty.")
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}

	getEmailQuery := "SELECT * from userInfo where email = ?"
	userRow := db.QueryRow(getEmailQuery, email)
	var uInfo UserInfo
	err = userRow.Scan(&uInfo.loginType, &uInfo.name, &uInfo.email, &uInfo.password)
	if err != nil && err != sql.ErrNoRows {
		log.Errorf("Unable to get data from db. %+v(%s)", err, err.Error())
	} else if err != sql.ErrNoRows {
		log.Infof("Email address %s is already registered. Please sign in.", email)
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}

	var pwdHash []byte
	pwdHash, err = bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		log.Infof("bcrypt err: %s", err.Error())
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}

	log.Debugf("Inserting info into userInfo table, %+v, %+v, %+v", uInfo, pwdHash, string(pwdHash))
	var insUserStmt *sql.Stmt
	insUserStmt, err = db.Prepare("INSERT INTO userInfo (login_type, name, email, password) VALUES (?, ?, ?, ?);")
	if err != nil {
		log.Infof("Error preparing statement. Error: %s", err.Error())
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	defer insUserStmt.Close()

	result, err := insUserStmt.Exec("local", name, email, pwdHash)
	rowsAff, _ := result.RowsAffected()
	lastIns, _ := result.LastInsertId()
	log.Debugf("Rows Affected: %+v, Last Inserted Id: %+v, err: %s", rowsAff, lastIns, err)
	if err != nil {
		log.Error("Error inserting new user")
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}

	log.Infof("New user inserted successfully, %+v, %+v", uInfo, string(pwdHash))

	http.Redirect(w, r, "/login", http.StatusFound)
}

func (fe *frontendServer) setCurrencyHandler(w http.ResponseWriter, r *http.Request) {
	log := r.Context().Value(ctxKeyLog{}).(logrus.FieldLogger)
	cur := r.FormValue("currency_code")
	log.WithField("curr.new", cur).WithField("curr.old", currentCurrency(r)).
		Debug("setting currency")

	if cur != "" {
		http.SetCookie(w, &http.Cookie{
			Name:   cookieCurrency,
			Value:  cur,
			MaxAge: cookieMaxAge,
		})
	}
	referer := r.Header.Get("referer")
	if referer == "" {
		referer = "/"
	}
	w.Header().Set("Location", referer)
	w.WriteHeader(http.StatusFound)
}

// chooseAd queries for advertisements available and randomly chooses one, if
// available. It ignores the error retrieving the ad since it is not critical.
func (fe *frontendServer) chooseAd(ctx context.Context, ctxKeys []string, log logrus.FieldLogger) *pb.Ad {
	ads, err := fe.getAd(ctx, ctxKeys)
	if err != nil {
		log.WithField("error", err).Warn("failed to retrieve ads")
		return nil
	}
	return ads[rand.Intn(len(ads))]
}

func renderHTTPError(log logrus.FieldLogger, r *http.Request, w http.ResponseWriter, err error, code int) {
	log.WithField("error", err).Error("request error")
	errMsg := fmt.Sprintf("%+v", err)

	w.WriteHeader(code)

	if templateErr := templates.ExecuteTemplate(w, "error", map[string]interface{}{
		"session_id":        sessionID(r),
		"request_id":        r.Context().Value(ctxKeyRequestID{}),
		"error":             errMsg,
		"status_code":       code,
		"status":            http.StatusText(code),
		"is_cymbal_brand":   isCymbalBrand,
		"deploymentDetails": deploymentDetailsMap,
	}); templateErr != nil {
		log.Println(templateErr)
	}
}

func currentCurrency(r *http.Request) string {
	c, _ := r.Cookie(cookieCurrency)
	if c != nil {
		return c.Value
	}
	return defaultCurrency
}

func sessionID(r *http.Request) string {
	v := r.Context().Value(ctxKeySessionID{})
	if v != nil {
		return v.(string)
	}
	return ""
}

func cartIDs(c []*pb.CartItem) []string {
	out := make([]string, len(c))
	for i, v := range c {
		out[i] = v.GetProductId()
	}
	return out
}

// get total # of items in cart
func cartSize(c []*pb.CartItem) int {
	cartSize := 0
	for _, item := range c {
		cartSize += int(item.GetQuantity())
	}
	return cartSize
}

func renderMoney(money pb.Money) string {
	currencyLogo := renderCurrencyLogo(money.GetCurrencyCode())
	return fmt.Sprintf("%s%d.%02d", currencyLogo, money.GetUnits(), money.GetNanos()/10000000)
}

func renderCurrencyLogo(currencyCode string) string {
	logos := map[string]string{
		"USD": "$",
		"CAD": "$",
		"JPY": "¥",
		"EUR": "€",
		"TRY": "₺",
		"GBP": "£",
	}

	logo := "$" //default
	if val, ok := logos[currencyCode]; ok {
		logo = val
	}
	return logo
}

func stringinSlice(slice []string, val string) bool {
	for _, item := range slice {
		if item == val {
			return true
		}
	}
	return false
}
