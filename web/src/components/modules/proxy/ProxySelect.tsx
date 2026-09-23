'use client';

import { useProxyList } from '@/api/endpoints/proxy';
import {
    Select,
    SelectContent,
    SelectItem,
    SelectTrigger,
    SelectValue,
} from '@/components/ui/select';
import { cn } from '@/lib/utils';

interface ProxySelectProps {
    value: number;
    onChange: (value: number) => void;
    /** 值为 0 时的选项文案（不选具体代理）。 */
    noneLabel: string;
    id?: string;
    className?: string;
    disabled?: boolean;
}

/**
 * 代理下拉框：选项来自代理管理页维护的代理列表，0 表示不指定代理。
 */
export function ProxySelect({ value, onChange, noneLabel, id, className, disabled }: ProxySelectProps) {
    const { data: proxies } = useProxyList();

    return (
        <Select value={String(value ?? 0)} onValueChange={(next) => onChange(Number(next))} disabled={disabled}>
            <SelectTrigger
                id={id}
                className={cn(
                    'rounded-xl w-full border border-border px-4 py-2 text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring',
                    className
                )}
            >
                <SelectValue />
            </SelectTrigger>
            <SelectContent className="rounded-xl">
                <SelectItem className="rounded-xl" value="0">{noneLabel}</SelectItem>
                {(proxies ?? []).map((proxy) => (
                    <SelectItem className="rounded-xl" value={String(proxy.id)} key={proxy.id}>
                        {proxy.name}
                    </SelectItem>
                ))}
            </SelectContent>
        </Select>
    );
}
