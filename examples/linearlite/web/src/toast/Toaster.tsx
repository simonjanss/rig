import { useNavigate } from "react-router";

import { useToasts } from "./ToastContext.js";

/** The stack in the corner. Clicking a toast follows it; × dismisses. */
export function Toaster() {
    const { toasts, dismiss } = useToasts();
    const navigate = useNavigate();

    if (toasts.length === 0) return null;
    return (
        <div className="toaster">
            {toasts.map((t) => (
                <div
                    key={t.id}
                    className={`toast toast-${t.kind}${t.to ? " toast-link" : ""}`}
                    onClick={() => {
                        if (t.to) {
                            void navigate(t.to);
                            dismiss(t.id);
                        }
                    }}
                >
                    <div className="toast-body">
                        <div className="toast-title">{t.title}</div>
                        {t.detail && (
                            <div className="toast-detail">{t.detail}</div>
                        )}
                    </div>
                    <button
                        className="toast-close"
                        aria-label="Dismiss"
                        onClick={(e) => {
                            e.stopPropagation();
                            dismiss(t.id);
                        }}
                    >
                        ×
                    </button>
                </div>
            ))}
        </div>
    );
}
