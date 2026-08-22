import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { BrowserRouter } from "react-router";

import { App } from "./App.js";
import { AuthProvider } from "./auth/AuthContext.js";
import { ToastProvider } from "./toast/ToastContext.js";

import "./styles/tokens.css";
import "./styles/base.css";
import "./styles/app.css";

createRoot(document.getElementById("root")!).render(
    <StrictMode>
        <BrowserRouter>
            <AuthProvider>
                <ToastProvider>
                    <App />
                </ToastProvider>
            </AuthProvider>
        </BrowserRouter>
    </StrictMode>,
);
