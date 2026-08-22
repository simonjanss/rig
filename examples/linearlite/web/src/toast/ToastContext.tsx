import type { ReactNode } from "react";

import {
    createContext,
    useCallback,
    useContext,
    useRef,
    useState,
} from "react";

export type Toast = {
    id: number;
    title: string;
    detail?: string;
    /** Where clicking it goes, a client-side route. */
    to?: string;
    kind: "info" | "error";
};

type Push = (t: Omit<Toast, "id">) => void;

const Ctx = createContext<{
    toasts: Toast[];
    push: Push;
    dismiss: (id: number) => void;
} | null>(null);

export function ToastProvider({ children }: { children: ReactNode }) {
    const [toasts, setToasts] = useState<Toast[]>([]);
    const next = useRef(1);

    const dismiss = useCallback((id: number) => {
        setToasts((ts) => ts.filter((t) => t.id !== id));
    }, []);

    const push = useCallback<Push>(
        (t) => {
            const id = next.current++;
            setToasts((ts) => [...ts.slice(-3), { ...t, id }]);
            window.setTimeout(() => dismiss(id), 6000);
        },
        [dismiss],
    );

    return (
        <Ctx.Provider value={{ toasts, push, dismiss }}>
            {children}
        </Ctx.Provider>
    );
}

export function useToasts() {
    const ctx = useContext(Ctx);
    if (!ctx) throw new Error("useToasts outside ToastProvider");
    return ctx;
}
