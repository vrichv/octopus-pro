'use client';

import React, {
  useCallback,
  useContext,
  useEffect,
  useId,
  useMemo,
  useRef,
  useState,
} from 'react';
import {
  motion,
  AnimatePresence,
  MotionConfig,
  Transition,
  Variant,
} from 'motion/react';
import { createPortal } from 'react-dom';
import { XIcon } from 'lucide-react';
import { cn } from '@/lib/utils';
import useClickOutside from '@/hooks/useClickOutside';

export type MorphingDialogContextType = {
  isOpen: boolean;
  setIsOpen: React.Dispatch<React.SetStateAction<boolean>>;
  uniqueId: string;
  triggerRef: React.RefObject<HTMLDivElement | null>;
  /** Ref — does NOT trigger re-renders. Set to true when the dialog has unsaved changes. */
  dirtyRef: React.MutableRefObject<boolean>;
};

const MorphingDialogContext =
  React.createContext<MorphingDialogContextType | null>(null);

function useMorphingDialog() {
  const context = useContext(MorphingDialogContext);
  if (!context) {
    throw new Error(
      'useMorphingDialog must be used within a MorphingDialogProvider'
    );
  }
  return context;
}

export type MorphingDialogProviderProps = {
  children: React.ReactNode;
  transition?: Transition;
};

function MorphingDialogProvider({
  children,
  transition,
}: MorphingDialogProviderProps) {
  const [isOpen, setIsOpen] = useState(false);
  const uniqueId = useId();
  const triggerRef = useRef<HTMLDivElement>(null!);
  const dirtyRef = useRef(false);

  const contextValue = useMemo(
    () => ({
      isOpen,
      setIsOpen,
      uniqueId,
      triggerRef,
      dirtyRef,
    }),
    [isOpen, uniqueId]
  );

  return (
    <MorphingDialogContext.Provider value={contextValue}>
      <MotionConfig transition={transition}>{children}</MotionConfig>
    </MorphingDialogContext.Provider>
  );
}

export type MorphingDialogProps = {
  children: React.ReactNode;
  transition?: Transition;
};

function MorphingDialog({ children, transition }: MorphingDialogProps) {
  return (
    <MorphingDialogProvider transition={transition}>
      {children}
    </MorphingDialogProvider>
  );
}

export type MorphingDialogTriggerProps = {
  children: React.ReactNode;
  className?: string;
  style?: React.CSSProperties;
  triggerRef?: React.RefObject<HTMLDivElement>;
};

function MorphingDialogTrigger({
  children,
  className,
  style,
  triggerRef: triggerRefProp,
}: MorphingDialogTriggerProps) {
  const { setIsOpen, isOpen, uniqueId, triggerRef } = useMorphingDialog();

  const handleClick = useCallback(() => {
    setIsOpen((open) => !open);
  }, [setIsOpen]);

  const handleKeyDown = useCallback(
    (event: React.KeyboardEvent<HTMLDivElement>) => {
      if (event.key === 'Enter' || event.key === ' ') {
        event.preventDefault();
        setIsOpen((open) => !open);
      }
    },
    [setIsOpen]
  );

  // When open, shared-layout can flash the trigger back into place during
  // internal re-layouts. Keep a non-motion placeholder in flow instead.
  if (isOpen) {
    return (
      <div
        ref={triggerRefProp ?? triggerRef}
        className={cn('relative', className)}
        style={{ ...style, visibility: 'hidden', pointerEvents: 'none' }}
        aria-hidden
      >
        {children}
      </div>
    );
  }

  return (
    <motion.div
      ref={triggerRefProp ?? triggerRef}
      layoutId={`dialog-${uniqueId}`}
      className={cn('relative cursor-pointer', className)}
      onClick={handleClick}
      onKeyDown={handleKeyDown}
      style={style}
      aria-haspopup='dialog'
      aria-expanded={isOpen}
      aria-controls={`motion-ui-morphing-dialog-content-${uniqueId}`}
      aria-label={`Open dialog ${uniqueId}`}
      role='button'
      tabIndex={0}
    >
      {children}
    </motion.div>
  );
}

export type MorphingDialogContentProps = {
  children: React.ReactNode;
  className?: string;
  style?: React.CSSProperties;
};

function MorphingDialogContent({
  children,
  className,
  style,
}: MorphingDialogContentProps) {
  const { setIsOpen, isOpen, dirtyRef, uniqueId, triggerRef } = useMorphingDialog();
  const containerRef = useRef<HTMLDivElement>(null!);
  const firstFocusableElementRef = useRef<HTMLElement | null>(null);
  const lastFocusableElementRef = useRef<HTMLElement | null>(null);

  const handleOutsideClick = useCallback(() => {
    if (!isOpen) return;
    // Dirty = has unsaved changes. Block outside click silently.
    if (dirtyRef.current) return;
    setIsOpen(false);
  }, [dirtyRef, isOpen, setIsOpen]);

  const shouldIgnoreOutsideClick = useCallback((event: Event) => {
    const target = event.target as HTMLElement | null;
    if (target?.closest('[data-slot="select-content"]')) return true;
    if (document.querySelector('[data-slot="select-content"]')) return true;
    if (target?.closest('[data-slot="popover-content"]')) return true;
    if (document.querySelector('[data-slot="popover-content"]')) return true;
    return false;
  }, []);

  useEffect(() => {
    const handleKeyDown = (event: KeyboardEvent) => {
      if (event.key === 'Escape') {
        if (dirtyRef.current) return;
        setIsOpen(false);
      }
      if (event.key === 'Tab') {
        if (!firstFocusableElementRef.current || !lastFocusableElementRef.current) return;

        if (event.shiftKey) {
          if (document.activeElement === firstFocusableElementRef.current) {
            event.preventDefault();
            lastFocusableElementRef.current.focus();
          }
        } else {
          if (document.activeElement === lastFocusableElementRef.current) {
            event.preventDefault();
            firstFocusableElementRef.current.focus();
          }
        }
      }
    };

    document.addEventListener('keydown', handleKeyDown);

    return () => {
      document.removeEventListener('keydown', handleKeyDown);
    };
  }, [dirtyRef, setIsOpen]);

  useEffect(() => {
    if (isOpen) {
      document.body.classList.add('overflow-hidden');
      const focusableElements = containerRef.current?.querySelectorAll(
        'button, [href], input, select, textarea, [tabindex]:not([tabindex="-1"])'
      );
      if (focusableElements && focusableElements.length > 0) {
        firstFocusableElementRef.current = focusableElements[0] as HTMLElement;
        lastFocusableElementRef.current = focusableElements[focusableElements.length - 1] as HTMLElement;
        (focusableElements[0] as HTMLElement).focus();
      }
    } else {
      document.body.classList.remove('overflow-hidden');
      triggerRef.current?.focus();
    }
  }, [isOpen, triggerRef]);

  useClickOutside(containerRef, handleOutsideClick, shouldIgnoreOutsideClick);

  // Sole owner of `dialog-${uniqueId}` while open. Trigger drops its layoutId
  // when open; Container must never take the same id or shared-layout collapses.
  return (
    <motion.div
      ref={containerRef}
      layoutId={`dialog-${uniqueId}`}
      className={cn('overflow-hidden', className)}
      style={style}
      role='dialog'
      aria-modal='true'
      aria-labelledby={`motion-ui-morphing-dialog-title-${uniqueId}`}
      aria-describedby={`motion-ui-morphing-dialog-description-${uniqueId}`}
    >
      {children}
    </motion.div>
  );
}

export type MorphingDialogContainerProps = {
  children: React.ReactNode;
  className?: string;
  style?: React.CSSProperties;
};

function MorphingDialogContainer({ children }: MorphingDialogContainerProps) {
  const { isOpen, uniqueId } = useMorphingDialog();
  const [mounted, setMounted] = useState(false);

  useEffect(() => {
    // Next tick avoids synchronous setState-in-effect warnings during SSR hydrate.
    const timer = setTimeout(() => setMounted(true), 0);
    return () => {
      clearTimeout(timer);
      setMounted(false);
    };
  }, []);

  if (!mounted) return null;

  return createPortal(
    <AnimatePresence initial={false} mode='sync'>
      {isOpen && (
        <>
          <motion.div
            key={`backdrop-${uniqueId}`}
            className='fixed inset-0 z-50 h-full w-full bg-white/40 backdrop-blur-xs dark:bg-black/40'
            initial={{ opacity: 0 }}
            animate={{ opacity: 1 }}
            exit={{ opacity: 0 }}
          />
          <div
            className='fixed inset-0 z-50 flex items-center justify-center p-4 sm:p-8'
            id={`motion-ui-morphing-dialog-content-${uniqueId}`}
          >
            {children}
          </div>
        </>
      )}
    </AnimatePresence>,
    document.body
  );
}

export type MorphingDialogTitleProps = {
  children: React.ReactNode;
  className?: string;
  style?: React.CSSProperties;
};

function MorphingDialogTitle({
  children,
  className,
  style,
}: MorphingDialogTitleProps) {
  const { uniqueId } = useMorphingDialog();

  return (
    <motion.div
      layoutId={`dialog-title-container-${uniqueId}`}
      className={className}
      style={style}
      layout
      id={`motion-ui-morphing-dialog-title-${uniqueId}`}
    >
      {children}
    </motion.div>
  );
}

export type MorphingDialogSubtitleProps = {
  children: React.ReactNode;
  className?: string;
  style?: React.CSSProperties;
};

function MorphingDialogSubtitle({
  children,
  className,
  style,
}: MorphingDialogSubtitleProps) {
  return (
    <motion.div
      className={cn('text-sm text-muted-foreground', className)}
      style={style}
    >
      {children}
    </motion.div>
  );
}

export type MorphingDialogDescriptionProps = {
  children: React.ReactNode;
  className?: string;
  variants?: {
    initial: Variant;
    animate: Variant;
    exit: Variant;
  };
  disableLayoutAnimation?: boolean;
  style?: React.CSSProperties;
};

function MorphingDialogDescription({
  children,
  className,
  variants,
  disableLayoutAnimation,
  style,
}: MorphingDialogDescriptionProps) {
  const { uniqueId } = useMorphingDialog();

  return (
    <motion.div
      key={`dialog-description-${uniqueId}`}
      layoutId={
        disableLayoutAnimation
          ? undefined
          : `dialog-description-content-${uniqueId}`
      }
      id={`motion-ui-morphing-dialog-description-${uniqueId}`}
      variants={variants}
      className={className}
      style={style}
      transition={{ delay: 0.1 }}
      initial='initial'
      animate='animate'
      exit='exit'
    >
      {children}
    </motion.div>
  );
}

export type MorphingDialogImageProps = {
  src: string;
  alt: string;
  className?: string;
  style?: React.CSSProperties;
};

function MorphingDialogImage({
  src,
  alt,
  className,
  style,
}: MorphingDialogImageProps) {
  return (
    <motion.img
      src={src}
      alt={alt}
      className={cn('rounded-2xl', className)}
      style={style}
    />
  );
}

export type MorphingDialogCloseProps = {
  children?: React.ReactNode;
  className?: string;
  variants?: {
    initial: Variant;
    animate: Variant;
    exit: Variant;
  };
};

function MorphingDialogClose({
  children,
  className,
  variants,
}: MorphingDialogCloseProps) {
  const { setIsOpen, uniqueId, dirtyRef } = useMorphingDialog();

  const handleClose = useCallback(() => {
    dirtyRef.current = false;
    setIsOpen(false);
  }, [dirtyRef, setIsOpen]);

  return (
    <motion.button
      onClick={handleClose}
      type='button'
      aria-label='Close dialog'
      key={`dialog-close-${uniqueId}`}
      className={cn('absolute top-6 right-6', className)}
      initial='initial'
      animate='animate'
      exit='exit'
      variants={variants}
    >
      {children || <XIcon size={24} />}
    </motion.button>
  );
}

export {
  MorphingDialog,
  MorphingDialogTrigger,
  MorphingDialogContainer,
  MorphingDialogContent,
  MorphingDialogClose,
  MorphingDialogTitle,
  MorphingDialogSubtitle,
  MorphingDialogDescription,
  MorphingDialogImage,
  useMorphingDialog,
};
