import {useEffect} from "react";
import { BrowserRouter as Router, Route, Switch } from "react-router-dom";
import { ToastContainer } from 'react-toastify';

import apiService from "./services/api.service";
import { AuthProvider } from "./common/useAuthContext";
import Role from "./common/Role";
import { PrivateRoute } from "./components/PrivateRoute";
import Sidebar from "./components/Sidebar";
import PasscodeResets from "./components/PasscodeResets";

import Login from "./pages/Login";
import Home from "./pages/Home";
import Connect from "./pages/Connect";
import Documents from "./pages/Documents";
import Integrations from "./pages/Integrations";
import Profile from "./pages/Profile";
import Admin from "./pages/Admin";
import ScreenShare from "./pages/ScreenShare";
import NoMatch from "./pages/404";

import "react-toastify/dist/ReactToastify.css";

import "./App.scss"

import { pdfjs } from "react-pdf";
pdfjs.GlobalWorkerOptions.workerSrc = new URL(
  'pdfjs-dist/build/pdf.worker.min.mjs',
  import.meta.url,
).toString(); 

export default function App() {

  useEffect(() => {
    apiService.checkLogin()
  }, [])

  return (
    <>
      <AuthProvider>
        <Router>
          <Switch>
            {/* Login page — no sidebar */}
            <Route path="/login" component={Login} />

            {/* All other routes — sidebar layout */}
            <Route>
              <div className="app-layout">
                <Sidebar />
                <PasscodeResets />
                <div className="main-content" style={{flex: "1 1 auto", minHeight: 0, overflow: "auto"}}>
                  <Switch>
                    <PrivateRoute exact path="/" component={Home} />
                    <PrivateRoute path="/documents/:itemId?" component={Documents} />
                    <PrivateRoute path="/connect" component={Connect} />
                    <PrivateRoute path="/pair/app" component={Connect} />
                    <PrivateRoute path="/pair" component={Connect} />
                    <PrivateRoute path="/integrations" component={Integrations} />
                    <PrivateRoute path="/profile" component={Profile} />
                    <PrivateRoute path="/screenshare" component={ScreenShare} />
                    <PrivateRoute path="/admin" roles={[Role.Admin]} component={Admin} />
                    <Route component={NoMatch} />
                  </Switch>
                </div>
              </div>
            </Route>
          </Switch>
        </Router>
      </AuthProvider>
      <ToastContainer autoClose={2000} />
    </>
  );
}